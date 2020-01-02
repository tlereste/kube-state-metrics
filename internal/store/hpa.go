/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package store

import (
	autoscaling "k8s.io/api/autoscaling/v2beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"k8s.io/kube-state-metrics/pkg/metric"
)

type MetricTargetType int

const (
	Value MetricTargetType = iota
	Utilization
	Average

	MetricTargetTypeCount // Used as a length argument to arrays
)

func (m MetricTargetType) String() string {
	return [...]string{"value", "utilization", "average"}[m]
}

var (
	descHorizontalPodAutoscalerLabelsName          = "kube_hpa_labels"
	descHorizontalPodAutoscalerLabelsHelp          = "Kubernetes labels converted to Prometheus labels."
	descHorizontalPodAutoscalerLabelsDefaultLabels = []string{"namespace", "hpa"}

	targetMetricLabels = []string{"metric_name", "metric_target_type"}

	hpaMetricFamilies = []metric.FamilyGenerator{
		{
			Name: "kube_hpa_metadata_generation",
			Type: metric.Gauge,
			Help: "The generation observed by the HorizontalPodAutoscaler controller.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				return &metric.Family{
					Metrics: []*metric.Metric{
						{
							Value: float64(a.ObjectMeta.Generation),
						},
					},
				}
			}),
		},
		{
			Name: "kube_hpa_spec_max_replicas",
			Type: metric.Gauge,
			Help: "Upper limit for the number of pods that can be set by the autoscaler; cannot be smaller than MinReplicas.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				return &metric.Family{
					Metrics: []*metric.Metric{
						{
							Value: float64(a.Spec.MaxReplicas),
						},
					},
				}
			}),
		},
		{
			Name: "kube_hpa_spec_min_replicas",
			Type: metric.Gauge,
			Help: "Lower limit for the number of pods that can be set by the autoscaler, default 1.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				return &metric.Family{
					Metrics: []*metric.Metric{
						{
							Value: float64(*a.Spec.MinReplicas),
						},
					},
				}
			}),
		},
		{
			Name: "kube_hpa_spec_target_metric",
			Type: metric.Gauge,
			Help: "The metric specifications used by this autoscaler when calculating the desired replica count.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				ms := make([]*metric.Metric, 0, len(a.Spec.Metrics))
				for _, m := range a.Spec.Metrics {
					var metricName string

					var v [MetricTargetTypeCount]int64
					var ok [MetricTargetTypeCount]bool

					switch m.Type {
					case autoscaling.ObjectMetricSourceType:
						metricName = m.Object.MetricName

						v[Value], ok[Value] = m.Object.TargetValue.AsInt64()
						if m.Object.AverageValue != nil {
							v[Average], ok[Average] = m.Object.AverageValue.AsInt64()
						}
					case autoscaling.PodsMetricSourceType:
						metricName = m.Pods.MetricName

						v[Average], ok[Average] = m.Pods.TargetAverageValue.AsInt64()
					case autoscaling.ResourceMetricSourceType:
						metricName = string(m.Resource.Name)

						if ok[Utilization] = (m.Resource.TargetAverageUtilization != nil); ok[Utilization] {
							v[Utilization] = int64(*m.Resource.TargetAverageUtilization)
						}

						if m.Resource.TargetAverageValue != nil {
							v[Average], ok[Average] = m.Resource.TargetAverageValue.AsInt64()
						}
					case autoscaling.ExternalMetricSourceType:
						metricName = m.External.MetricName

						// The TargetValue and TargetAverageValue are mutually exclusive
						if m.External.TargetValue != nil {
							v[Value], ok[Value] = m.External.TargetValue.AsInt64()
						}
						if m.External.TargetAverageValue != nil {
							v[Average], ok[Average] = m.External.TargetAverageValue.AsInt64()
						}
					default:
						// Skip unsupported metric type
						continue
					}

					for i := range ok {
						if ok[i] {
							ms = append(ms, &metric.Metric{
								LabelKeys:   targetMetricLabels,
								LabelValues: []string{metricName, MetricTargetType(i).String()},
								Value:       float64(v[i]),
							})
						}
					}
				}
				return &metric.Family{Metrics: ms}
			}),
		},
		{
			Name: "kube_hpa_status_current_replicas",
			Type: metric.Gauge,
			Help: "Current number of replicas of pods managed by this autoscaler.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				return &metric.Family{
					Metrics: []*metric.Metric{
						{
							Value: float64(a.Status.CurrentReplicas),
						},
					},
				}
			}),
		},
		{
			Name: "kube_hpa_status_desired_replicas",
			Type: metric.Gauge,
			Help: "Desired number of replicas of pods managed by this autoscaler.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				return &metric.Family{
					Metrics: []*metric.Metric{
						{
							Value: float64(a.Status.DesiredReplicas),
						},
					},
				}
			}),
		},
		{
			Name: descHorizontalPodAutoscalerLabelsName,
			Type: metric.Gauge,
			Help: descHorizontalPodAutoscalerLabelsHelp,
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				labelKeys, labelValues := kubeLabelsToPrometheusLabels(a.Labels)
				return &metric.Family{
					Metrics: []*metric.Metric{
						{
							LabelKeys:   labelKeys,
							LabelValues: labelValues,
							Value:       1,
						},
					},
				}
			}),
		},
		{
			Name: "kube_hpa_status_condition",
			Type: metric.Gauge,
			Help: "The condition of this autoscaler.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				ms := make([]*metric.Metric, len(a.Status.Conditions)*len(conditionStatuses))

				for i, c := range a.Status.Conditions {
					metrics := addConditionMetrics(c.Status)

					for j, m := range metrics {
						metric := m
						metric.LabelKeys = []string{"condition", "status"}
						metric.LabelValues = append([]string{string(c.Type)}, metric.LabelValues...)
						ms[i*len(conditionStatuses)+j] = metric
					}
				}

				return &metric.Family{
					Metrics: ms,
				}
			}),
		},
		{
			Name: "kube_hpa_status_current_metrics_average_value",
			Type: metric.Gauge,
			Help: "Average metric value observed by the autoscaler.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				ms := make([]*metric.Metric, len(a.Status.CurrentMetrics))
				for i, c := range a.Status.CurrentMetrics {
					var value *resource.Quantity
					switch c.Type {
					case autoscaling.ResourceMetricSourceType:
						value = &c.Resource.CurrentAverageValue
					case autoscaling.PodsMetricSourceType:
						value = &c.Pods.CurrentAverageValue
					case autoscaling.ObjectMetricSourceType:
						value = c.Object.AverageValue
					case autoscaling.ExternalMetricSourceType:
						value = c.External.CurrentAverageValue
					default:
						// Skip unsupported metric type
						continue
					}
					var metricValue float64
					if c.Type == autoscaling.ResourceMetricSourceType && c.Resource.Name == corev1.ResourceCPU {
						metricValue = float64(value.MilliValue()) / 1000
					} else if intVal, canFastConvert := value.AsInt64(); canFastConvert {
						metricValue = float64(intVal)
					} else {
						// Skip unsupported metric value format
						continue
					}
					ms[i] = &metric.Metric{
						Value: metricValue,
					}
				}
				return &metric.Family{
					Metrics: ms,
				}
			}),
		},
		{
			Name: "kube_hpa_status_current_metrics_average_utilization",
			Type: metric.Gauge,
			Help: "Average metric utilization observed by the autoscaler.",
			GenerateFunc: wrapHPAFunc(func(a *autoscaling.HorizontalPodAutoscaler) *metric.Family {
				ms := make([]*metric.Metric, len(a.Status.CurrentMetrics))
				for i, c := range a.Status.CurrentMetrics {
					if c.Type == autoscaling.ResourceMetricSourceType {
						ms[i] = &metric.Metric{
							Value: float64(*c.Resource.CurrentAverageUtilization),
						}
					}
				}
				return &metric.Family{
					Metrics: ms,
				}
			}),
		},
	}
)

func wrapHPAFunc(f func(*autoscaling.HorizontalPodAutoscaler) *metric.Family) func(interface{}) *metric.Family {
	return func(obj interface{}) *metric.Family {
		hpa := obj.(*autoscaling.HorizontalPodAutoscaler)

		metricFamily := f(hpa)

		for _, m := range metricFamily.Metrics {
			m.LabelKeys = append(descHorizontalPodAutoscalerLabelsDefaultLabels, m.LabelKeys...)
			m.LabelValues = append([]string{hpa.Namespace, hpa.Name}, m.LabelValues...)
		}

		return metricFamily
	}
}

func createHPAListWatch(kubeClient clientset.Interface, ns string) cache.ListerWatcher {
	return &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			return kubeClient.AutoscalingV2beta1().HorizontalPodAutoscalers(ns).List(opts)
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
			return kubeClient.AutoscalingV2beta1().HorizontalPodAutoscalers(ns).Watch(opts)
		},
	}
}
