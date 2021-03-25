// Copyright 2020 The Operator-SDK Authors
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

package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"text/template"
	"time"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/operator-framework/operator-sdk/internal/olm/operator"
	"github.com/operator-framework/operator-sdk/internal/util/k8sutil"
)

const (
	// defaultGRPCPort is the default grpc container port that the registry pod exposes
	defaultGRPCPort = 50051
	defaultDBPath   = "/database/index.db"

	defaultContainerName     = "registry-grpc"
	defaultContainerPortName = "grpc"
)

// BundleItem contains the metadata of a bundle image relevant to the registry pod.
type BundleItem struct {
	// ImageTag is the bundle image's tag.
	ImageTag string `json:"imageTag"`
	// AddMode describes how the bundle should be added to an index image.
	AddMode BundleAddMode `json:"mode"`
}

// RegistryPod holds resources necessary for creation of a registry server
type RegistryPod struct {
	// BundleItems contains all bundles to be added to a registry pod.
	BundleItems []BundleItem

	// Index image contains a database of pointers to operator manifest content that is queriable via an API.
	// new version of an operator bundle when published can be added to an index image
	IndexImage string

	// DBPath refers to the registry DB;
	// if an index image is provided, the existing registry DB is located at /database/index.db
	DBPath string

	// GRPCPort is the container grpc port
	GRPCPort int32

	// SecretName holds the name of an image pull secret to mount into the Pod so `opm registry add`
	// can pull bundle images from a private registry.
	SecretName string

	// pod represents a kubernetes *corev1.pod that will be created on a cluster using an index image
	pod *corev1.Pod

	cfg *operator.Configuration
}

// init initializes the RegistryPod struct and sets defaults for empty fields
func (rp *RegistryPod) init(cfg *operator.Configuration) error {
	if rp.GRPCPort == 0 {
		rp.GRPCPort = defaultGRPCPort
	}
	if rp.DBPath == "" {
		rp.DBPath = defaultDBPath
	}
	rp.cfg = cfg

	// validate the RegistryPod struct and ensure required fields are set
	if err := rp.validate(); err != nil {
		return fmt.Errorf("invalid registry pod: %v", err)
	}

	// podForBundleRegistry() to make the pod definition
	pod, err := rp.podForBundleRegistry()
	if err != nil {
		return fmt.Errorf("error building registry pod definition: %v", err)
	}
	rp.pod = pod

	return nil
}

// Create creates a bundle registry pod built from an index image,
// sets the catalog source as the owner for the pod and verifies that
// the pod is running
func (rp *RegistryPod) Create(ctx context.Context, cfg *operator.Configuration, cs *v1alpha1.CatalogSource) (*corev1.Pod, error) {
	if err := rp.init(cfg); err != nil {
		return nil, err
	}

	// make catalog source the owner of registry pod object
	if err := controllerutil.SetOwnerReference(cs, rp.pod, rp.cfg.Scheme); err != nil {
		return nil, fmt.Errorf("set registry pod owner reference: %v", err)
	}

	if err := rp.cfg.Client.Create(ctx, rp.pod); err != nil {
		return nil, fmt.Errorf("create registry pod: %v", err)
	}

	// get registry pod key
	podKey := types.NamespacedName{
		Namespace: rp.cfg.Namespace,
		Name:      rp.pod.GetName(),
	}

	// poll and verify that pod is running
	podCheck := wait.ConditionFunc(func() (done bool, err error) {
		err = rp.cfg.Client.Get(ctx, podKey, rp.pod)
		if err != nil {
			return false, fmt.Errorf("error getting pod %s: %w", rp.pod.Name, err)
		}
		return rp.pod.Status.Phase == corev1.PodRunning, nil
	})

	// check pod status to be `Running`
	if err := rp.checkPodStatus(ctx, podCheck); err != nil {
		return nil, fmt.Errorf("registry pod did not become ready: %w", err)
	}
	log.Infof("Successfully created registry pod: %s", rp.pod.Name)
	return rp.pod, nil
}

// checkPodStatus polls and verifies that the pod status is running
func (rp *RegistryPod) checkPodStatus(ctx context.Context, podCheck wait.ConditionFunc) error {
	// poll every 200 ms until podCheck is true or context is done
	err := wait.PollImmediateUntil(200*time.Millisecond, podCheck, ctx.Done())
	if err != nil {
		return fmt.Errorf("error waiting for registry pod %s to run: %v", rp.pod.Name, err)
	}

	return err
}

// validate will ensure that RegistryPod required fields are set
// and throws error if not set
func (rp *RegistryPod) validate() error {
	if len(rp.BundleItems) == 0 {
		return errors.New("bundle image set cannot be empty")
	}
	for _, item := range rp.BundleItems {
		if item.ImageTag == "" {
			return errors.New("bundle image cannot be empty")
		}
		if err := item.AddMode.Validate(); err != nil {
			return fmt.Errorf("invalid bundle add mode: %v", err)
		}
	}

	if rp.IndexImage == "" {
		return errors.New("index image cannot be empty")
	}

	return nil
}

func GetRegistryPodHost(ipStr string) string {
	return fmt.Sprintf("%s:%d", ipStr, defaultGRPCPort)
}

// getPodName will return a string constructed from the bundle Image name
func getPodName(bundleImage string) string {
	// todo(rashmigottipati): need to come up with human-readable references
	// to be able to handle SHA references in the bundle images
	return k8sutil.TrimDNS1123Label(k8sutil.FormatOperatorNameDNS1123(bundleImage))
}

// podForBundleRegistry constructs and returns the registry pod definition
// and throws error when unable to build the pod definition successfully
func (rp *RegistryPod) podForBundleRegistry() (*corev1.Pod, error) {
	// rp was already validated so len(rp.BundleItems) must be greater than 0.
	bundleImage := rp.BundleItems[len(rp.BundleItems)-1].ImageTag

	// construct the container command for pod spec
	containerCmd, err := rp.getContainerCmd()
	if err != nil {
		return nil, err
	}

	// make the pod definition
	rp.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      getPodName(bundleImage),
			Namespace: rp.cfg.Namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  defaultContainerName,
					Image: rp.IndexImage,
					Command: []string{
						"/bin/sh",
						"-c",
						containerCmd,
					},
					Ports: []corev1.ContainerPort{
						{Name: defaultContainerPortName, ContainerPort: rp.GRPCPort},
					},
				},
			},
			ServiceAccountName: rp.cfg.ServiceAccount,
		},
	}

	applySecret(rp.pod, rp.SecretName)

	return rp.pod, nil
}

// applySecret creates a docker config volume for secretName
// and a volumeMount for that secret in each container in pod.
func applySecret(pod *corev1.Pod, secretName string) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: secretName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  secretName,
				DefaultMode: newInt32(0400),
				Optional:    newBool(false),
				// Require a non-legacy docker config secret.
				Items: []corev1.KeyToPath{
					{
						Key:  ".dockerconfigjson",
						Path: ".docker/config.json",
					},
				},
			},
		},
	})

	volumeMount := corev1.VolumeMount{
		Name:     secretName,
		ReadOnly: true,
		// Mount in $HOME.
		MountPath: "/root",
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, volumeMount)
	}
}

func newInt32(i int32) *int32 {
	ip := new(int32)
	*ip = i
	return ip
}

func newBool(b bool) *bool {
	bp := new(bool)
	*bp = b
	return bp
}

const containerCommand = `/bin/mkdir -p {{ dirname .DBPath }} && \
{{- range $i, $item := .BundleItems }}
/bin/opm registry add -d {{ $.DBPath }} -b {{ $item.ImageTag }} --mode={{ $item.AddMode }} && \
{{- end }}
/bin/opm registry serve -d {{ .DBPath }} -p {{ .GRPCPort }}
`

// getContainerCmd uses templating to construct the container command
// and throws error if unable to parse and execute the container command
func (rp *RegistryPod) getContainerCmd() (string, error) {

	// create a custom dirname template function
	funcMap := template.FuncMap{
		"dirname": path.Dir,
	}

	// add the custom dirname template function to the
	// template's FuncMap and parse the containerCommand
	tmp := template.Must(template.New("cmd").Funcs(funcMap).Parse(containerCommand))

	// execute the command by applying the parsed tmp to command
	// and write command output to out
	out := &bytes.Buffer{}
	if err := tmp.Execute(out, rp); err != nil {
		return "", fmt.Errorf("parse container command: %w", err)
	}

	return out.String(), nil
}
