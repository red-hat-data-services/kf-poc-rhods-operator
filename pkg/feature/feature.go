package feature

import (
	"context"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = ctrlLog.Log.WithName("features")

type Feature struct {
	Name    string
	Spec    *Spec
	Enabled bool

	Clientset     *kubernetes.Clientset
	DynamicClient dynamic.Interface
	Client        client.Client

	manifests      []manifest
	cleanups       []Action
	resources      []Action
	preconditions  []Action
	postconditions []Action
	loaders        []Action
}

// Action is a func type which can be used for different purposes while having access to Feature struct.
type Action func(feature *Feature) error

func (f *Feature) Apply() error {
	if !f.Enabled {
		return nil
	}

	if err := f.createResourceTracker(); err != nil {
		return err
	}

	// Verify all precondition and collect errors
	var multiErr *multierror.Error
	for _, precondition := range f.preconditions {
		multiErr = multierror.Append(multiErr, precondition(f))
	}

	if multiErr.ErrorOrNil() != nil {
		return multiErr.ErrorOrNil()
	}

	// Load necessary data
	for _, loader := range f.loaders {
		multiErr = multierror.Append(multiErr, loader(f))
	}
	if multiErr.ErrorOrNil() != nil {
		return multiErr.ErrorOrNil()
	}

	// create or update resources
	for _, resource := range f.resources {
		if err := resource(f); err != nil {
			return err
		}
	}

	// Process and apply manifests
	for _, m := range f.manifests {
		if err := m.processTemplate(f.Spec); err != nil {
			return errors.WithStack(err)
		}

		log.Info("converted template to manifest", "feature", f.Name, "path", m.targetPath())
	}

	if err := f.applyManifests(); err != nil {
		return err
	}

	for _, postcondition := range f.postconditions {
		multiErr = multierror.Append(multiErr, postcondition(f))
	}

	if multiErr.ErrorOrNil() != nil {
		return multiErr.ErrorOrNil()
	}

	return nil
}

func (f *Feature) Cleanup() error {
	if !f.Enabled {
		return nil
	}

	// Ensure associated FeatureTracker instance has been removed as last one
	// in the chain of cleanups.
	f.addCleanup(removeFeatureTracker)

	var cleanupErrors *multierror.Error
	for _, cleanupFunc := range f.cleanups {
		cleanupErrors = multierror.Append(cleanupErrors, cleanupFunc(f))
	}

	return cleanupErrors.ErrorOrNil()
}

func (f *Feature) applyManifests() error {
	var applyErrors *multierror.Error
	for _, m := range f.manifests {
		applyErrors = multierror.Append(applyErrors, f.apply(m))
	}

	return applyErrors.ErrorOrNil()
}

func (f *Feature) CreateConfigMap(cfgMapName string, data map[string]string) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfgMapName,
			Namespace: f.Spec.AppNamespace,
			OwnerReferences: []metav1.OwnerReference{
				f.AsOwnerReference(),
			},
		},
		Data: data,
	}

	configMaps := f.Clientset.CoreV1().ConfigMaps(configMap.Namespace)
	_, err := configMaps.Get(context.TODO(), configMap.Name, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) { //nolint:gocritic
		_, err = configMaps.Create(context.TODO(), configMap, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	} else if k8serrors.IsAlreadyExists(err) {
		_, err = configMaps.Update(context.TODO(), configMap, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	} else {
		return err
	}

	return nil
}

func (f *Feature) addCleanup(cleanupFuncs ...Action) {
	f.cleanups = append(f.cleanups, cleanupFuncs...)
}

type apply func(filename string) error

func (f *Feature) apply(m manifest) error {
	var applier apply
	targetPath := m.targetPath()

	if m.patch {
		applier = func(filename string) error {
			log.Info("patching using manifest", "feature", f.Name, "name", m.name, "path", targetPath)

			return f.patchResourceFromFile(filename)
		}
	} else {
		applier = func(filename string) error {
			log.Info("applying manifest", "feature", f.Name, "name", m.name, "path", targetPath)

			return f.createResourceFromFile(filename)
		}
	}

	if err := applier(targetPath); err != nil {
		log.Error(err, "failed to create resource", "feature", f.Name, "name", m.name, "path", targetPath)

		return err
	}

	return nil
}

func (f *Feature) AsOwnerReference() metav1.OwnerReference {
	return f.Spec.Tracker.ToOwnerReference()
}
