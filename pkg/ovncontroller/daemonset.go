/*
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

package ovncontroller

import (
	"fmt"
	"strings"

	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	"github.com/openstack-k8s-operators/lib-common/modules/common/tls"
	ovnv1 "github.com/openstack-k8s-operators/ovn-operator/api/v1beta1"
	ovn_common "github.com/openstack-k8s-operators/ovn-operator/pkg/common"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// DaemonSet func
func DaemonSet(
	instance *ovnv1.OVNController,
	configHash string,
	labels map[string]string,
	annotations map[string]string,
) (*appsv1.DaemonSet, error) {

	runAsUser := int64(0)
	privileged := true

	volumes := GetVolumes(instance.Name, instance.Namespace)
	commonVolumeMounts := []corev1.VolumeMount{}

	// add CA bundle if defined
	if instance.Spec.TLS.CaBundleSecretName != "" {
		volumes = append(volumes, instance.Spec.TLS.CreateVolume())
		commonVolumeMounts = append(commonVolumeMounts, instance.Spec.TLS.CreateVolumeMounts(nil)...)
	}

	ovnControllerVolumeMounts := append(GetOvnControllerVolumeMounts(), commonVolumeMounts...)

	// add OVN dbs cert and CA
	var ovnControllerTLSArgs []string
	if instance.Spec.TLS.Enabled() {
		svc := tls.Service{
			SecretName: *instance.Spec.TLS.GenericService.SecretName,
			CertMount:  ptr.To(ovn_common.OVNDbCertPath),
			KeyMount:   ptr.To(ovn_common.OVNDbKeyPath),
			CaMount:    ptr.To(ovn_common.OVNDbCaCertPath),
		}
		volumes = append(volumes, svc.CreateVolume(ovnv1.ServiceNameOvnController))
		ovnControllerVolumeMounts = append(ovnControllerVolumeMounts, svc.CreateVolumeMounts(ovnv1.ServiceNameOvnController)...)
		ovnControllerTLSArgs = []string{
			fmt.Sprintf("--certificate=%s", ovn_common.OVNDbCertPath),
			fmt.Sprintf("--private-key=%s", ovn_common.OVNDbKeyPath),
			fmt.Sprintf("--ca-cert=%s", ovn_common.OVNDbCaCertPath),
		}
	}

	//
	// https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
	//
	ovsDbLivenessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      5,
		PeriodSeconds:       3,
		InitialDelaySeconds: 3,
	}

	ovsVswitchdLivenessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      5,
		PeriodSeconds:       3,
		InitialDelaySeconds: 3,
	}

	var ovsDbPreStopCmd []string
	var ovsDbCmd []string
	var ovsDbArgs []string

	var ovsVswitchdCmd []string
	var ovsVswitchdArgs []string
	var ovsVswitchdPreStopCmd []string

	var ovnControllerCmd []string
	var ovnControllerArgs []string
	var ovnControllerPreStopCmd []string

	ovsDbLivenessProbe.Exec = &corev1.ExecAction{
		Command: []string{
			"/usr/bin/ovs-vsctl",
			"show",
		},
	}
	ovsDbCmd = []string{
		"/usr/bin/dumb-init",
	}
	ovsDbArgs = []string{
		"--single-child", "--", "/usr/local/bin/container-scripts/start-ovsdb-server.sh",
	}
	// sleep is required as workaround for https://github.com/kubernetes/kubernetes/issues/39170
	ovsDbPreStopCmd = []string{
		"/usr/share/openvswitch/scripts/ovs-ctl", "stop", "--no-ovs-vswitchd", ";", "sleep", "2",
	}

	ovsVswitchdLivenessProbe.Exec = &corev1.ExecAction{
		Command: []string{
			"/usr/bin/ovs-appctl",
			"bond/show",
		},
	}
	ovsVswitchdCmd = []string{
		"/usr/sbin/ovs-vswitchd",
	}
	ovsVswitchdArgs = []string{
		"--pidfile", "--mlockall",
	}
	// sleep is required as workaround for https://github.com/kubernetes/kubernetes/issues/39170
	ovsVswitchdPreStopCmd = []string{
		"/usr/share/openvswitch/scripts/ovs-ctl", "stop", "--no-ovsdb-server", ";", "sleep", "2",
	}

	ovnControllerCmd = []string{
		"/bin/bash", "-c",
	}
	ovnControllerArgs = []string{
		strings.Join(
			append(
				[]string{"/usr/local/bin/container-scripts/net_setup.sh && ovn-controller"},
				append(ovnControllerTLSArgs, "--pidfile", "unix:/run/openvswitch/db.sock")...,
			),
			" ",
		),
	}
	// sleep is required as workaround for https://github.com/kubernetes/kubernetes/issues/39170
	ovnControllerPreStopCmd = []string{
		"/usr/share/ovn/scripts/ovn-ctl", "stop_controller", ";", "sleep", "2",
	}

	envVars := map[string]env.Setter{}
	envVars["CONFIG_HASH"] = env.SetValue(configHash)

	daemonset := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ovnv1.ServiceNameOvnController,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: instance.RbacResourceName(),
					Containers: []corev1.Container{
						// ovsdb-server container
						{
							Name:    "ovsdb-server",
							Command: ovsDbCmd,
							Args:    ovsDbArgs,
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: ovsDbPreStopCmd,
									},
								},
							},
							Image: instance.Spec.OvsContainerImage,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add:  []corev1.Capability{"NET_ADMIN", "SYS_ADMIN", "SYS_NICE"},
									Drop: []corev1.Capability{},
								},
								RunAsUser:  &runAsUser,
								Privileged: &privileged,
							},
							Env:                      env.MergeEnvs([]corev1.EnvVar{}, envVars),
							VolumeMounts:             append(GetOvsDbVolumeMounts(), commonVolumeMounts...),
							Resources:                instance.Spec.Resources,
							LivenessProbe:            ovsDbLivenessProbe,
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						}, {
							// ovs-vswitchd container
							Name:    "ovs-vswitchd",
							Command: ovsVswitchdCmd,
							Args:    ovsVswitchdArgs,
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: ovsVswitchdPreStopCmd,
									},
								},
							},
							Image: instance.Spec.OvsContainerImage,
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add:  []corev1.Capability{"NET_ADMIN", "SYS_ADMIN", "SYS_NICE"},
									Drop: []corev1.Capability{},
								},
								RunAsUser:  &runAsUser,
								Privileged: &privileged,
							},
							Env:                      env.MergeEnvs([]corev1.EnvVar{}, envVars),
							VolumeMounts:             append(GetVswitchdVolumeMounts(), commonVolumeMounts...),
							Resources:                instance.Spec.Resources,
							LivenessProbe:            ovsVswitchdLivenessProbe,
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						}, {
							// ovn-controller container
							// NOTE(slaweq): for some reason, when ovn-controller is started without
							// bash shell, it fails with error "unrecognized option --pidfile"
							Name:    "ovn-controller",
							Command: ovnControllerCmd,
							Args:    ovnControllerArgs,
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: ovnControllerPreStopCmd,
									},
								},
							},
							Image: instance.Spec.OvnContainerImage,
							// TODO(slaweq): to check if ovn-controller really needs such security contexts
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add:  []corev1.Capability{"NET_ADMIN", "SYS_ADMIN", "SYS_NICE"},
									Drop: []corev1.Capability{},
								},
								RunAsUser:  &runAsUser,
								Privileged: &privileged,
							},
							Env:                      env.MergeEnvs([]corev1.EnvVar{}, envVars),
							VolumeMounts:             ovnControllerVolumeMounts,
							Resources:                instance.Spec.Resources,
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						},
					},
				},
			},
		},
	}
	daemonset.Spec.Template.Spec.Volumes = volumes

	if instance.Spec.NodeSelector != nil && len(instance.Spec.NodeSelector) > 0 {
		daemonset.Spec.Template.Spec.NodeSelector = instance.Spec.NodeSelector
	}

	return daemonset, nil

}
