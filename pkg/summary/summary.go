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

package summary

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/klog"

	"github.com/fairwindsops/goldilocks/pkg/utils"
)

const (
	namespaceAllNamespaces = ""
)

// Summary is for storing a summary of recommendation data by namespace/deployment/container
type Summary struct {
	Namespaces map[string]namespaceSummary
}

type namespaceSummary struct {
	Namespace string                     `json:"namespace"`
	Workloads map[string]workloadSummary `json:"workloads"`
}

type workloadSummary struct {
	WorkloadName string                      `json:"workloadName"`
	Kind         string                      `json:"kind"`
	Containers   map[string]containerSummary `json:"containers"`
}

type containerSummary struct {
	ContainerName string `json:"containerName"`

	// recommendations
	LowerBound     corev1.ResourceList `json:"lowerBound"`
	UpperBound     corev1.ResourceList `json:"upperBound"`
	Target         corev1.ResourceList `json:"target"`
	UncappedTarget corev1.ResourceList `json:"uncappedTarget"`
	Limits         corev1.ResourceList `json:"limits"`
	Requests       corev1.ResourceList `json:"requests"`
}

// Summarizer represents a source of generating a summary of VPAs
type Summarizer struct {
	options

	// cached list of vpas
	vpas []vpav1.VerticalPodAutoscaler

	// cached map of workload/vpa name -> workload
	workloadForVPANamed map[string]*workload
}

// workload represents any pod generating workload, that
// can be watched by a VPA
// (ie. deployment, stateful set, daemonset)
type workload struct {
	metav1.TypeMeta
	metav1.ObjectMeta
	containers []corev1.Container
}

// VPAName produces a VPA name base on the workload name and kind
// <workload-name>-<workload-kind>
func (w workload) VPAName() string {
	return fmt.Sprintf("%s-%s", w.Name, strings.ToLower(w.TypeMeta.Kind))
}

// NewSummarizer returns a Summarizer for all goldilocks managed VPAs in all Namespaces
func NewSummarizer(setters ...Option) *Summarizer {
	opts := defaultOptions()
	for _, setter := range setters {
		setter(opts)
	}

	return &Summarizer{
		options: *opts,
	}
}

// NewSummarizerForVPAs returns a Summarizer for a known list of VPAs
func NewSummarizerForVPAs(vpas []vpav1.VerticalPodAutoscaler, setters ...Option) *Summarizer {
	summarizer := NewSummarizer(setters...)

	// set the cached vpas list directly
	summarizer.vpas = vpas

	return summarizer
}

// GetSummary returns a Summary of the Summarizer using its options
func (s Summarizer) GetSummary() (Summary, error) {
	// blank summary
	summary := Summary{
		Namespaces: map[string]namespaceSummary{},
	}

	// if the summarizer is filtering for a single namespace,
	// then add that namespace by default to the blank summary
	if s.namespace != namespaceAllNamespaces {
		summary.Namespaces[s.namespace] = namespaceSummary{
			Namespace: s.namespace,
			Workloads: map[string]workloadSummary{},
		}
	}

	// cached vpas and deployments
	if s.vpas == nil || s.workloadForVPANamed == nil {
		err := s.Update()
		if err != nil {
			return summary, err
		}
	}

	// nothing to summarize
	if len(s.vpas) <= 0 {
		return summary, nil
	}

	for _, vpa := range s.vpas {
		klog.V(8).Infof("Analyzing vpa: %v", vpa.Name)

		// get or create the namespaceSummary for this VPA's namespace
		namespace := vpa.Namespace
		var nsSummary namespaceSummary
		if val, ok := summary.Namespaces[namespace]; ok {
			nsSummary = val
		} else {
			nsSummary = namespaceSummary{
				Namespace: namespace,
				Workloads: map[string]workloadSummary{},
			}
			summary.Namespaces[namespace] = nsSummary
		}

		dSummary := workloadSummary{
			WorkloadName: vpa.Name,
			Kind:         vpa.Spec.TargetRef.Kind,
			Containers:   map[string]containerSummary{},
		}

		workload, ok := s.workloadForVPANamed[vpa.Name]
		if !ok {
			klog.Errorf("no matching workload found for VPA/%s", vpa.Name)
			continue
		}

		if vpa.Status.Recommendation == nil {
			klog.V(2).Infof("Empty status on %v", dSummary.WorkloadName)
			continue
		}
		if len(vpa.Status.Recommendation.ContainerRecommendations) <= 0 {
			klog.V(2).Infof("No recommendations found in the %v vpa.", dSummary.WorkloadName)
			continue
		}

		// get the full set of excluded containers for this Deployment
		excludedContainers := sets.NewString().Union(s.excludedContainers)
		if val, exists := workload.GetAnnotations()[utils.DeploymentExcludeContainersAnnotation]; exists {
			excludedContainers.Insert(strings.Split(val, ",")...)
		}

	CONTAINER_REC_LOOP:
		for _, containerRecommendation := range vpa.Status.Recommendation.ContainerRecommendations {
			if excludedContainers.Has(containerRecommendation.ContainerName) {
				klog.V(2).Infof("Excluding container Deployment/%s/%s", dSummary.WorkloadName, containerRecommendation.ContainerName)
				continue CONTAINER_REC_LOOP
			}

			var cSummary containerSummary
			for _, c := range workload.containers {
				// find the matching container on the deployment
				if c.Name == containerRecommendation.ContainerName {
					cSummary = containerSummary{
						ContainerName:  containerRecommendation.ContainerName,
						UpperBound:     utils.FormatResourceList(containerRecommendation.UpperBound),
						LowerBound:     utils.FormatResourceList(containerRecommendation.LowerBound),
						Target:         utils.FormatResourceList(containerRecommendation.Target),
						UncappedTarget: utils.FormatResourceList(containerRecommendation.UncappedTarget),
						Limits:         utils.FormatResourceList(c.Resources.Limits),
						Requests:       utils.FormatResourceList(c.Resources.Requests),
					}
					klog.V(6).Infof("Resources for Deployment/%s/%s: Requests: %v Limits: %v", dSummary.WorkloadName, c.Name, cSummary.Requests, cSummary.Limits)
					dSummary.Containers[cSummary.ContainerName] = cSummary
					continue CONTAINER_REC_LOOP
				}
			}
		}

		// update summary maps
		nsSummary.Workloads[dSummary.WorkloadName] = dSummary
		summary.Namespaces[nsSummary.Namespace] = nsSummary
	}

	return summary, nil
}

// Update the set of VPAs and Deployments that the Summarizer uses for creating a summary
func (s *Summarizer) Update() error {
	err := s.updateVPAs()
	if err != nil {
		klog.Error(err.Error())
		return err
	}

	err = s.updateWorkloads()
	if err != nil {
		klog.Error(err.Error())
		return err
	}

	return nil
}

func (s *Summarizer) updateVPAs() error {
	nsLog := s.namespace
	if s.namespace == namespaceAllNamespaces {
		nsLog = "all namespaces"
	}
	klog.V(3).Infof("Looking for VPAs in %s with labels: %v", nsLog, s.vpaLabels)
	vpas, err := s.listVPAs(getVPAListOptionsForLabels(s.vpaLabels))
	if err != nil {
		return err
	}
	klog.V(10).Infof("Found vpas: %v", vpas)

	s.vpas = vpas
	return nil
}

func (s Summarizer) listVPAs(listOptions metav1.ListOptions) ([]vpav1.VerticalPodAutoscaler, error) {
	vpas, err := s.vpaClient.Client.AutoscalingV1().VerticalPodAutoscalers(s.namespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}

	return vpas.Items, nil
}

func getVPAListOptionsForLabels(vpaLabels map[string]string) metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: labels.Set(vpaLabels).String(),
	}
}

func (s *Summarizer) updateWorkloads() error {
	nsLog := s.namespace
	if s.namespace == namespaceAllNamespaces {
		nsLog = "all namespaces"
	}
	klog.V(3).Infof("Looking for workloads in %s", nsLog)
	workloads, err := s.listWorkloads(metav1.ListOptions{})
	if err != nil {
		return err
	}
	klog.V(10).Infof("Found workloads: %v", workloads)

	// map the workload.vpaName -> &workload for easy vpa lookup
	s.workloadForVPANamed = map[string]*workload{}
	for _, d := range workloads {
		d := d
		s.workloadForVPANamed[d.VPAName()] = &d
	}

	return nil
}

func (s Summarizer) listWorkloads(listOptions metav1.ListOptions) ([]workload, error) {
	workloadLen := 0
	deployments, err := s.listDeployments(listOptions)
	if err != nil {
		return nil, err
	}

	workloadLen += len(deployments)

	statefulSets, err := s.listStatefulSets(listOptions)
	if err != nil {
		return nil, err
	}

	workloadLen += len(statefulSets)

	workloads := make([]workload, 0, workloadLen)

	for _, deployment := range deployments {
		workloads = append(
			workloads,
			workload{
				TypeMeta:   deployment.TypeMeta,
				ObjectMeta: deployment.ObjectMeta,
				containers: deployment.Spec.Template.Spec.Containers,
			})
	}

	for _, statefulset := range statefulSets {
		workloads = append(
			workloads,
			workload{
				TypeMeta:   statefulset.TypeMeta,
				ObjectMeta: statefulset.ObjectMeta,
				containers: statefulset.Spec.Template.Spec.Containers,
			})
	}

	return workloads, nil
}

func (s Summarizer) listDeployments(listOptions metav1.ListOptions) ([]appsv1.Deployment, error) {
	deployments, err := s.kubeClient.Client.AppsV1().Deployments(s.namespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}

	return deployments.Items, nil
}

func (s Summarizer) listStatefulSets(listOptions metav1.ListOptions) ([]appsv1.StatefulSet, error) {
	statefulsets, err := s.kubeClient.Client.AppsV1().StatefulSets(s.namespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}

	return statefulsets.Items, nil
}
