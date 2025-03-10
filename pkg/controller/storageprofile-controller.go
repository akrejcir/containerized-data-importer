package controller

import (
	"context"
	"fmt"
	"reflect"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/monitoring"
	"kubevirt.io/containerized-data-importer/pkg/operator"
	"kubevirt.io/containerized-data-importer/pkg/storagecapabilities"
	"kubevirt.io/containerized-data-importer/pkg/util"
)

var (
	// IncompleteProfileGauge is the metric we use to alert about incomplete storage profiles
	IncompleteProfileGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: monitoring.MetricOptsList[monitoring.IncompleteProfile].Name,
			Help: monitoring.MetricOptsList[monitoring.IncompleteProfile].Help,
		})
)

// StorageProfileReconciler members
type StorageProfileReconciler struct {
	client client.Client
	// use this for getting any resources not in the install namespace or cluster scope
	uncachedClient  client.Client
	scheme          *runtime.Scheme
	log             logr.Logger
	installerLabels map[string]string
}

// Reconcile the reconcile.Reconciler implementation for the StorageProfileReconciler object.
func (r *StorageProfileReconciler) Reconcile(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := r.log.WithValues("StorageProfile", req.NamespacedName)
	log.Info("reconciling StorageProfile")

	storageClass := &storagev1.StorageClass{}
	if err := r.client.Get(context.TODO(), req.NamespacedName, storageClass); err != nil {
		if k8serrors.IsNotFound(err) {
			return reconcile.Result{}, r.deleteStorageProfile(req.NamespacedName.Name, log)
		}
		return reconcile.Result{}, err
	} else if storageClass.GetDeletionTimestamp() != nil {
		return reconcile.Result{}, r.deleteStorageProfile(req.NamespacedName.Name, log)
	}

	if _, err := r.reconcileStorageProfile(storageClass); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, r.checkIncompleteProfiles()
}

func (r *StorageProfileReconciler) reconcileStorageProfile(sc *storagev1.StorageClass) (reconcile.Result, error) {
	log := r.log.WithValues("StorageProfile", sc.Name)

	storageProfile, prevStorageProfile, err := r.getStorageProfile(sc)
	if err != nil {
		log.Error(err, "Unable to create StorageProfile")
		return reconcile.Result{}, err
	}

	storageProfile.Status.StorageClass = &sc.Name
	storageProfile.Status.Provisioner = &sc.Provisioner
	storageProfile.Status.CloneStrategy = r.reconcileCloneStrategy(sc, storageProfile.Spec.CloneStrategy)

	var claimPropertySets []cdiv1.ClaimPropertySet

	if len(storageProfile.Spec.ClaimPropertySets) > 0 {
		for _, cps := range storageProfile.Spec.ClaimPropertySets {
			if len(cps.AccessModes) == 0 && cps.VolumeMode != nil {
				err = fmt.Errorf("must provide access mode for volume mode: %s", *cps.VolumeMode)
				log.Error(err, "Unable to update StorageProfile")
				return reconcile.Result{}, err
			}
		}
		claimPropertySets = storageProfile.Spec.ClaimPropertySets
	} else {
		claimPropertySets = r.reconcilePropertySets(sc)
	}

	storageProfile.Status.ClaimPropertySets = claimPropertySets

	util.SetRecommendedLabels(storageProfile, r.installerLabels, "cdi-controller")
	if err := r.updateStorageProfile(prevStorageProfile, storageProfile, log); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *StorageProfileReconciler) updateStorageProfile(prevStorageProfile runtime.Object, storageProfile *cdiv1.StorageProfile, log logr.Logger) error {
	if prevStorageProfile == nil {
		return r.client.Create(context.TODO(), storageProfile)
	} else if !reflect.DeepEqual(prevStorageProfile, storageProfile) {
		// Updates have happened, update StorageProfile.
		log.Info("Updating StorageProfile", "StorageProfile.Name", storageProfile.Name, "storageProfile", storageProfile)
		return r.client.Update(context.TODO(), storageProfile)
	}

	return nil
}

func (r *StorageProfileReconciler) getStorageProfile(sc *storagev1.StorageClass) (*cdiv1.StorageProfile, runtime.Object, error) {
	var prevStorageProfile runtime.Object
	storageProfile := &cdiv1.StorageProfile{}

	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: sc.Name}, storageProfile); err != nil {
		if k8serrors.IsNotFound(err) {
			storageProfile, err = r.createEmptyStorageProfile(sc)
			if err != nil {
				return nil, nil, err
			}
		} else {
			return nil, nil, err
		}
	} else {
		prevStorageProfile = storageProfile.DeepCopyObject()
	}

	return storageProfile, prevStorageProfile, nil
}

func (r *StorageProfileReconciler) reconcilePropertySets(sc *storagev1.StorageClass) []cdiv1.ClaimPropertySet {
	claimPropertySets := []cdiv1.ClaimPropertySet{}
	capabilities, found := storagecapabilities.Get(r.client, sc)
	if found {
		for i := range capabilities {
			claimPropertySet := cdiv1.ClaimPropertySet{
				AccessModes: []v1.PersistentVolumeAccessMode{capabilities[i].AccessMode},
				VolumeMode:  &capabilities[i].VolumeMode,
			}
			claimPropertySets = append(claimPropertySets, claimPropertySet)
		}
	}
	return claimPropertySets
}

func (r *StorageProfileReconciler) reconcileCloneStrategy(sc *storagev1.StorageClass, clonestrategy *cdiv1.CDICloneStrategy) *cdiv1.CDICloneStrategy {

	if clonestrategy == nil {
		if sc.Annotations["cdi.kubevirt.io/clone-strategy"] == "copy" {
			strategy := cdiv1.CloneStrategyHostAssisted
			return &strategy
		} else if sc.Annotations["cdi.kubevirt.io/clone-strategy"] == "snapshot" {
			strategy := cdiv1.CloneStrategySnapshot
			return &strategy
		} else if sc.Annotations["cdi.kubevirt.io/clone-strategy"] == "csi-clone" {
			strategy := cdiv1.CloneStrategyCsiClone
			return &strategy
		} else {
			return clonestrategy
		}
	}
	return clonestrategy
}

func (r *StorageProfileReconciler) createEmptyStorageProfile(sc *storagev1.StorageClass) (*cdiv1.StorageProfile, error) {
	storageProfile := MakeEmptyStorageProfileSpec(sc.Name)
	util.SetRecommendedLabels(storageProfile, r.installerLabels, "cdi-controller")
	// uncachedClient is used to directly get the resource, SetOwnerRuntime requires some cluster-scoped resources
	// normal/cached client does list resource, a cdi user might not have the rights to list cluster scope resource
	if err := operator.SetOwnerRuntime(r.uncachedClient, storageProfile); err != nil {
		return nil, err
	}
	return storageProfile, nil
}

func (r *StorageProfileReconciler) deleteStorageProfile(name string, log logr.Logger) error {
	log.Info("Cleaning up StorageProfile that corresponds to deleted StorageClass", "StorageClass.Name", name)
	storageProfileObj := &cdiv1.StorageProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	if err := r.client.Delete(context.TODO(), storageProfileObj); IgnoreNotFound(err) != nil {
		return err
	}

	return r.checkIncompleteProfiles()
}

func isNoProvisioner(name string, cl client.Client) bool {
	storageClass := &storagev1.StorageClass{}
	if err := cl.Get(context.TODO(), types.NamespacedName{Name: name}, storageClass); err != nil {
		return false
	}
	return storageClass.Provisioner == "kubernetes.io/no-provisioner"
}

func (r *StorageProfileReconciler) checkIncompleteProfiles() error {
	numIncomplete := 0
	storageProfileList := &cdiv1.StorageProfileList{}
	if err := r.client.List(context.TODO(), storageProfileList); err != nil {
		return err
	}
	for _, profile := range storageProfileList.Items {
		if isIncomplete(profile.Status.ClaimPropertySets) {
			numIncomplete++
		}
	}
	IncompleteProfileGauge.Set(float64(numIncomplete))

	return nil
}

// MakeEmptyStorageProfileSpec creates StorageProfile manifest
func MakeEmptyStorageProfileSpec(name string) *cdiv1.StorageProfile {
	return &cdiv1.StorageProfile{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StorageProfile",
			APIVersion: "cdi.kubevirt.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				common.CDILabelKey:       common.CDILabelValue,
				common.CDIComponentLabel: "",
			},
		},
	}
}

// NewStorageProfileController creates a new instance of the StorageProfile controller.
func NewStorageProfileController(mgr manager.Manager, log logr.Logger, installerLabels map[string]string) (controller.Controller, error) {
	uncachedClient, err := client.New(mgr.GetConfig(), client.Options{
		Scheme: mgr.GetScheme(),
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		return nil, err
	}
	reconciler := &StorageProfileReconciler{
		client:          mgr.GetClient(),
		uncachedClient:  uncachedClient,
		scheme:          mgr.GetScheme(),
		log:             log.WithName("storageprofile-controller"),
		installerLabels: installerLabels,
	}

	storageProfileController, err := controller.New(
		"storageprofile-controller",
		mgr,
		controller.Options{Reconciler: reconciler})
	if err != nil {
		return nil, err
	}
	if err := addStorageProfileControllerWatches(mgr, storageProfileController, log); err != nil {
		return nil, err
	}

	log.Info("Initialized StorageProfile controller")
	return storageProfileController, nil
}

func addStorageProfileControllerWatches(mgr manager.Manager, c controller.Controller, log logr.Logger) error {
	// Add schemes.
	if err := cdiv1.AddToScheme(mgr.GetScheme()); err != nil {
		return err
	}
	if err := storagev1.AddToScheme(mgr.GetScheme()); err != nil {
		return err
	}

	if err := c.Watch(&source.Kind{Type: &storagev1.StorageClass{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}
	if err := c.Watch(&source.Kind{Type: &cdiv1.StorageProfile{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}
	if err := c.Watch(&source.Kind{Type: &v1.PersistentVolume{}}, handler.EnqueueRequestsFromMapFunc(
		func(obj client.Object) []reconcile.Request {
			return []reconcile.Request{{
				NamespacedName: types.NamespacedName{Name: scName(obj)},
			}}
		},
	),
		predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return isNoProvisioner(scName(e.Object), mgr.GetClient()) },
			UpdateFunc: func(e event.UpdateEvent) bool { return isNoProvisioner(scName(e.ObjectNew), mgr.GetClient()) },
			DeleteFunc: func(e event.DeleteEvent) bool { return isNoProvisioner(scName(e.Object), mgr.GetClient()) },
		}); err != nil {
		return err
	}
	return nil
}

func scName(obj client.Object) string {
	return obj.(*v1.PersistentVolume).Spec.StorageClassName
}

func isIncomplete(sets []cdiv1.ClaimPropertySet) bool {
	if len(sets) > 0 {
		for _, cps := range sets {
			if len(cps.AccessModes) == 0 || cps.VolumeMode == nil {
				return true
			}
		}
	} else {
		return true
	}

	return false
}
