// Copyright 2019 FairwindsOps Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vpa

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/util/retry"

	autoscaling "k8s.io/api/autoscaling/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/fairwindsops/goldilocks/pkg/kube"
	"github.com/fairwindsops/goldilocks/pkg/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/klog"
)

type workload struct {
	metav1.TypeMeta
	metav1.ObjectMeta
}

// VPAName produces a VPA name base on the workload name and kind
// <workload-name>-<workload-kind>
func (w workload) VPAName() string {
	return fmt.Sprintf("%s-%s", w.Name, strings.ToLower(w.TypeMeta.Kind))
}

// Reconciler checks if VPA objects should be created or deleted
type Reconciler struct {
	KubeClient        *kube.ClientInstance
	VPAClient         *kube.VPAClientInstance
	OnByDefault       bool
	DryRun            bool
	IncludeNamespaces []string
	ExcludeNamespaces []string
}

var singleton *Reconciler

// GetInstance returns a Reconciler singleton
func GetInstance() *Reconciler {
	if singleton == nil {
		singleton = &Reconciler{
			KubeClient: kube.GetInstance(),
			VPAClient:  kube.GetVPAInstance(),
		}
	}
	return singleton
}

// SetInstance sets the singleton using preconstructed k8s and vpa clients. Used for testing.
func SetInstance(k8s *kube.ClientInstance, vpa *kube.VPAClientInstance) *Reconciler {
	singleton = &Reconciler{
		KubeClient: k8s,
		VPAClient:  vpa,
	}
	return singleton
}

// ReconcileNamespace makes a vpa for every deployment in the namespace.
// Check if deployment has label for false before applying vpa.
func (r Reconciler) ReconcileNamespace(namespace *corev1.Namespace) error {
	nsName := namespace.ObjectMeta.Name
	vpas, err := r.listVPAs(nsName)
	if err != nil {
		klog.Error(err.Error())
		return err
	}

	if !r.namespaceIsManaged(namespace) {
		klog.V(2).Infof("Namespace/%s is not managed, cleaning up VPAs...", namespace.Name)
		// Namespaced used to be managed, but isn't anymore. Delete all of the
		// VPAs that we control.
		return r.cleanUpManagedVPAsInNamespace(nsName, vpas)
	}

	workloads, err := r.listWorkloads(nsName)

	if err != nil {
		klog.Error(err.Error())
		return err
	}

	return r.reconcileWorkloadsAndVPAs(namespace, vpas, workloads)
}

func (r Reconciler) cleanUpManagedVPAsInNamespace(namespace string, vpas []vpav1.VerticalPodAutoscaler) error {
	if len(vpas) < 1 {
		klog.V(4).Infof("No goldilocks managed VPAs found in Namespace/%s, skipping cleanup", namespace)
		return nil
	}
	klog.Infof("Deleting all goldilocks managed VPAs in Namespace/%s", namespace)
	for _, vpa := range vpas {
		err := r.deleteVPA(vpa)
		if err != nil {
			return err
		}
	}
	return nil
}

// NOTE: This is not used right now.  Deployments have been scrapped.
// Keeping this here for future development.
func (r Reconciler) checkDeploymentLabels(deployment *appsv1.Deployment) (bool, error) {
	if len(deployment.ObjectMeta.Labels) > 0 {
		for k, v := range deployment.ObjectMeta.Labels {
			klog.V(7).Infof("Deployment Label - %s: %s", k, v)
			if strings.ToLower(k) == utils.VpaEnabledLabel {
				return strconv.ParseBool(v)
			}
		}
	}
	return false, nil
}

func (r Reconciler) namespaceIsManaged(namespace *corev1.Namespace) bool {
	for k, v := range namespace.ObjectMeta.Labels {
		klog.V(4).Infof("Namespace/%s found label: %s=%s", namespace.Name, k, v)
		if strings.ToLower(k) != utils.VpaEnabledLabel {
			klog.V(9).Infof("Namespace/%s with label key %s does not match enabled label %s", namespace.Name, k, utils.VpaEnabledLabel)
			continue
		}
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			klog.Errorf("Found unsupported value for Namespace/%s label %s=%s, defaulting to false", namespace.Name, k, v)
			return false
		}
		return enabled
	}

	for _, included := range r.IncludeNamespaces {
		if namespace.ObjectMeta.Name == included {
			return true
		}
	}
	for _, excluded := range r.ExcludeNamespaces {
		if namespace.ObjectMeta.Name == excluded {
			return false
		}
	}

	return r.OnByDefault
}

func (r Reconciler) reconcileWorkloadsAndVPAs(ns *corev1.Namespace, vpas []vpav1.VerticalPodAutoscaler, workloads []workload) error {
	defaultUpdateMode, _ := vpaUpdateModeForResource(ns)
	// these keys will eventually contain the leftover vpas that do not have a matching deployment associated
	vpaHasAssociatedDeployment := map[string]bool{}
	for _, workload := range workloads {
		vpaName := workload.VPAName()
		var wvpa *vpav1.VerticalPodAutoscaler
		// search for the matching vpa (will have the <workload name>-<workload-type>)
		for idx, vpa := range vpas {
			if vpaName == vpa.Name && workload.Kind == vpa.Spec.TargetRef.Kind && workload.APIVersion == vpa.Spec.TargetRef.APIVersion {
				// found the vpa associated with this workload
				wvpa = &vpas[idx]
				vpaHasAssociatedDeployment[wvpa.Name] = true
				break
			}
		}

		if wvpa != nil {
			vpaName = wvpa.Name
		}
		klog.V(2).Infof("Reconciling Namespace/%s for %s/%s with VPA/%s", ns.Name, workload.Kind, workload.Name, vpaName)
		err := r.reconcileWorkloadAndVPA(ns, workload, wvpa, defaultUpdateMode)
		if err != nil {
			return err
		}
	}

	for _, vpa := range vpas {
		if !vpaHasAssociatedDeployment[vpa.Name] {
			// these vpas do not have a matching deployment, delete them
			klog.V(2).Infof("Deleting dangling VPA/%s in Namespace/%s", vpa.Name, ns.Name)
			err := r.deleteVPA(vpa)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (r Reconciler) reconcileWorkloadAndVPA(ns *corev1.Namespace, workload workload, vpa *vpav1.VerticalPodAutoscaler, vpaUpdateMode *vpav1.UpdateMode) error {
	vpaName := workloadVPAName(workload)
	desiredVPA := r.getWorkloadVPAObject(vpa, workload, ns, vpaName, vpaUpdateMode)

	if vpaUpdateModeOverride, explicit := vpaUpdateModeForWorkload(workload); explicit {
		vpaUpdateMode = vpaUpdateModeOverride
		klog.V(5).Infof("%s/%s has custom vpa-update-mode=%s", workload.GetObjectKind(), workload.Name, *vpaUpdateMode)
	}
	if vpa == nil {
		klog.V(5).Infof("%s/%s does not have a VPA currently, creating VPA/%s", workload.GetObjectKind(), workload.Name, workload.Name)
		// no vpa exists, create one (use the same name as the deployment)
		err := r.createVPA(desiredVPA)
		if err != nil {
			return err
		}
	} else {
		// vpa exists
		klog.V(5).Infof("%s/%s has a VPA currently, updating VPA/%s", workload.GetObjectKind(), workload.Name, workload.Name)
		err := r.updateVPA(desiredVPA)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r Reconciler) listWorkloads(namespace string) ([]workload, error) {
	deployments, err := r.listDeployments(namespace)
	if err != nil {
		return nil, err
	}

	workloads := make([]workload, 0, len(deployments))

	for _, deployment := range deployments {
		workloads = append(
			workloads,
			workload{
				TypeMeta:   deployment.TypeMeta,
				ObjectMeta: deployment.ObjectMeta,
			})
	}

	return workloads, nil
}

func (r Reconciler) listDeployments(namespace string) ([]appsv1.Deployment, error) {
	deployments, err := r.KubeClient.Client.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	klog.V(2).Infof("There are %d deployments in Namespace/%s", len(deployments.Items), namespace)
	if klog.V(9) {
		for _, d := range deployments.Items {
			klog.V(9).Infof("Found Deployment/%s in Namespace/%s", d.Name, namespace)
		}
	}

	return deployments.Items, nil
}

func (r Reconciler) listVPAs(namespace string) ([]vpav1.VerticalPodAutoscaler, error) {
	vpaListOptions := metav1.ListOptions{
		LabelSelector: labels.Set(utils.VPALabels).String(),
	}
	existingVPAs, err := r.VPAClient.Client.AutoscalingV1().VerticalPodAutoscalers(namespace).List(context.TODO(), vpaListOptions)
	if err != nil {
		return nil, err
	}

	klog.V(2).Infof("There are %d vpas in Namespace/%s", len(existingVPAs.Items), namespace)
	if klog.V(9) {
		for _, vpa := range existingVPAs.Items {
			klog.V(9).Infof("Found VPA/%s in Namespace/%s", vpa.Name, namespace)
		}
	}

	return existingVPAs.Items, nil
}

func (r Reconciler) deleteVPA(vpa vpav1.VerticalPodAutoscaler) error {
	if r.DryRun {
		klog.Infof("Not deleting VPA/%s due to dryrun.", vpa.Name)
		return nil
	}

	errDelete := r.VPAClient.Client.AutoscalingV1().VerticalPodAutoscalers(vpa.Namespace).Delete(context.TODO(), vpa.Name, metav1.DeleteOptions{})
	if errDelete != nil {
		klog.Errorf("Error deleting VPA/%s in Namespace/%s: %v", vpa.Name, vpa.Namespace, errDelete)
		return errDelete
	}
	klog.Infof("Deleted VPA/%s in Namespace/%s", vpa.Name, vpa.Namespace)
	return nil
}

func (r Reconciler) createVPA(vpa vpav1.VerticalPodAutoscaler) error {
	if !r.DryRun {
		klog.V(9).Infof("Creating VPA/%s: %v", vpa.Name, vpa)
		_, err := r.VPAClient.Client.AutoscalingV1().VerticalPodAutoscalers(vpa.Namespace).Create(context.TODO(), &vpa, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("Error creating VPA/%s in Namespace/%s: %v", vpa.Name, vpa.Namespace, err)
			return err
		}
		klog.Infof("Created VPA/%s in Namespace/%s", vpa.Name, vpa.Namespace)
	} else {
		klog.Infof("Not creating VPA/%s in Namespace/%s due to dryrun.", vpa.Name, vpa.Namespace)
	}
	return nil
}

func (r Reconciler) updateVPA(vpa vpav1.VerticalPodAutoscaler) error {
	if !r.DryRun {
		klog.V(9).Infof("Updating VPA/%s: %v", vpa.Name, vpa)
		// attempt to update the vpa using retries and backoffs
		// [See: https://github.com/kubernetes/client-go/blob/master/examples/create-update-delete-deployment/main.go#L125]
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Note: Normally we're supposed to be getting the current VPA object, then updating that object between
			//       each retry attempt, but since goldilocks should be the only controller that is manipulating
			//       these VPA objects then it's safe to use the desired VPA that is originally passed to this function.
			_, err := r.VPAClient.Client.AutoscalingV1().VerticalPodAutoscalers(vpa.Namespace).Update(context.TODO(), &vpa, metav1.UpdateOptions{})
			return err
		})
		if retryErr != nil {
			klog.Errorf("Error updating VPA/%s in Namespace/%s: %v", vpa.Name, vpa.Namespace, retryErr)
			return retryErr
		}
		klog.Infof("Updated VPA/%s in Namespace/%s", vpa.Name, vpa.Namespace)
	} else {
		klog.Infof("Not updating VPA/%s in Namespace/%s due to dryrun.", vpa.Name, vpa.Namespace)
	}
	return nil
}

func (r Reconciler) getVPAObject(existingVPA *vpav1.VerticalPodAutoscaler, ns *corev1.Namespace, vpaName string, updateMode *vpav1.UpdateMode) vpav1.VerticalPodAutoscaler {
	var desiredVPA vpav1.VerticalPodAutoscaler

	// create a brand new vpa with the correct information
	if existingVPA == nil {
		desiredVPA = vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vpaName,
				Namespace: ns.Name,
			},
		}
	} else {
		// or use the existing VPA as a template to update from
		desiredVPA = *existingVPA
	}

	// update the labels on the VPA
	desiredVPA.Labels = utils.VPALabels

	// update the spec on the VPA
	desiredVPA.Spec = vpav1.VerticalPodAutoscalerSpec{
		TargetRef: &autoscaling.CrossVersionObjectReference{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       vpaName,
		},
		UpdatePolicy: &vpav1.PodUpdatePolicy{
			UpdateMode: updateMode,
		},
	}

	return desiredVPA
}

func (r Reconciler) getWorkloadVPAObject(existingVPA *vpav1.VerticalPodAutoscaler, wl workload, ns *corev1.Namespace, vpaName string, updateMode *vpav1.UpdateMode) vpav1.VerticalPodAutoscaler {
	var desiredVPA vpav1.VerticalPodAutoscaler
	vpaName := wl.VPAName()

	// create a brand new vpa with the correct information
	if existingVPA == nil {
		desiredVPA = vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vpaName,
				Namespace: ns.Name,
			},
		}
	} else {
		// or use the existing VPA as a template to update from
		desiredVPA = *existingVPA
	}

	// update the labels on the VPA
	desiredVPA.Labels = utils.VPALabels

	// update the spec on the VPA
	desiredVPA.Spec = vpav1.VerticalPodAutoscalerSpec{
		TargetRef: &autoscaling.CrossVersionObjectReference{
			APIVersion: wl.APIVersion,
			Kind:       wl.Kind,
			Name:       wl.Name,
		},
		UpdatePolicy: &vpav1.PodUpdatePolicy{
			UpdateMode: updateMode,
		},
	}

	return desiredVPA
}

// vpaUpdateModeForResource searches the resource's annotations and labels for a vpa-update-mode
// key/value and uses that key/value to return the proper UpdateMode type
func vpaUpdateModeForResource(obj runtime.Object) (*vpav1.UpdateMode, bool) {
	requestedVPAMode := vpav1.UpdateModeOff
	explicit := false

	requestStr := ""
	accessor, _ := meta.Accessor(obj)
	if val, ok := accessor.GetAnnotations()[utils.VpaUpdateModeKey]; ok {
		requestStr = val
	} else if val, ok := accessor.GetLabels()[utils.VpaUpdateModeKey]; ok {
		requestStr = val
	}
	if requestStr != "" {
		requestStr = strings.ToUpper(requestStr[0:1]) + strings.ToLower(requestStr[1:])
		requestedVPAMode = vpav1.UpdateMode(requestStr)
		explicit = true
	}

	return &requestedVPAMode, explicit
}

// vpaUpdateModeForWorkload searches the workloads's annotations and labels for a vpa-update-mode
// key/value and uses that key/value to return the proper UpdateMode type
func vpaUpdateModeForWorkload(wl workload) (*vpav1.UpdateMode, bool) {
	requestedVPAMode := vpav1.UpdateModeOff
	explicit := false

	requestStr := ""
	if val, ok := wl.GetAnnotations()[utils.VpaUpdateModeKey]; ok {
		requestStr = val
	} else if val, ok := wl.GetLabels()[utils.VpaUpdateModeKey]; ok {
		requestStr = val
	}
	if requestStr != "" {
		requestStr = strings.ToUpper(requestStr[0:1]) + strings.ToLower(requestStr[1:])
		requestedVPAMode = vpav1.UpdateMode(requestStr)
		explicit = true
	}

	return &requestedVPAMode, explicit
}
