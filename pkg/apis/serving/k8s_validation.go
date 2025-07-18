/*
Copyright 2019 The Knative Authors

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

package serving

import (
	"context"
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/observability/runtime"
	"knative.dev/serving/pkg/apis/config"
	"knative.dev/serving/pkg/networking"
)

const (
	minUserID, maxUserID   = 0, math.MaxInt32
	minGroupID, maxGroupID = 0, math.MaxInt32
)

var (
	reservedPaths = sets.New(
		"/",
		"/dev",
		"/dev/log", // Should be a domain socket
	)

	reservedContainerNames = sets.New(
		"queue-proxy",
	)

	reservedEnvVars = sets.New(
		"PORT",
		"K_SERVICE",
		"K_CONFIGURATION",
		"K_REVISION",
	)

	reservedPorts = sets.NewInt32(
		networking.BackendHTTPPort,
		networking.BackendHTTP2Port,
		networking.QueueAdminPort,
		networking.AutoscalingQueueMetricsPort,
		networking.UserQueueMetricsPort,
		runtime.ProfilingDefaultPort)

	reservedSidecarEnvVars = reservedEnvVars.Difference(sets.New("PORT"))

	// The port is named "user-port" on the deployment, but a user cannot set an arbitrary name on the port
	// in Configuration. The name field is reserved for content-negotiation. Currently 'h2c' and 'http1' are
	// allowed.
	// https://github.com/knative/serving/blob/main/docs/runtime-contract.md#inbound-network-connectivity
	validPortNames = sets.New(
		"h2c",
		"http1",
		"",
	)
)

// ValidateVolumes validates the Volumes of a PodSpec.
func ValidateVolumes(ctx context.Context, vs []corev1.Volume, mountedVolumes sets.Set[string]) (map[string]corev1.Volume, *apis.FieldError) {
	volumes := make(map[string]corev1.Volume, len(vs))
	var errs *apis.FieldError
	for i, volume := range vs {
		if _, ok := volumes[volume.Name]; ok {
			errs = errs.Also((&apis.FieldError{
				Message: fmt.Sprintf("duplicate volume name %q", volume.Name),
				Paths:   []string{"name"},
			}).ViaIndex(i))
		}
		if !mountedVolumes.Has(volume.Name) {
			errs = errs.Also((&apis.FieldError{
				Message: fmt.Sprintf("volume with name %q not mounted", volume.Name),
				Paths:   []string{"name"},
			}).ViaIndex(i))
		}
		errs = errs.Also(validateVolume(ctx, volume).ViaIndex(i))
		volumes[volume.Name] = volume
	}
	return volumes, errs
}

func validatePersistentVolumeClaims(volume corev1.VolumeSource, features *config.Features) *apis.FieldError {
	var errs *apis.FieldError
	if volume.PersistentVolumeClaim == nil {
		return nil
	}
	if features.PodSpecPersistentVolumeClaim != config.Enabled {
		errs = errs.Also(&apis.FieldError{Message: fmt.Sprintf("Persistent volume claim support is disabled, "+
			"but found persistent volume claim %s", volume.PersistentVolumeClaim.ClaimName)})
	}
	isWriteEnabled := features.PodSpecPersistentVolumeWrite == config.Enabled
	if !volume.PersistentVolumeClaim.ReadOnly && !isWriteEnabled {
		errs = errs.Also(&apis.FieldError{Message: fmt.Sprintf("Persistent volume write support is disabled, "+
			"but found persistent volume claim %s that is not read-only", volume.PersistentVolumeClaim.ClaimName)})
	}
	return errs
}

func validateVolume(ctx context.Context, volume corev1.Volume) *apis.FieldError {
	features := config.FromContextOrDefaults(ctx).Features
	errs := validatePersistentVolumeClaims(volume.VolumeSource, features)
	if volume.Image != nil && features.PodSpecVolumesImage != config.Enabled {
		errs = errs.Also(&apis.FieldError{Message: fmt.Sprintf("Image volume support is disabled, "+
			"but found Image volume %s", volume.Name)})
	}
	if volume.EmptyDir != nil && features.PodSpecVolumesEmptyDir != config.Enabled {
		errs = errs.Also(&apis.FieldError{Message: fmt.Sprintf("EmptyDir volume support is disabled, "+
			"but found EmptyDir volume %s", volume.Name)})
	}
	if volume.HostPath != nil && features.PodSpecVolumesHostPath != config.Enabled {
		errs = errs.Also(&apis.FieldError{Message: fmt.Sprintf("HostPath volume support is disabled, "+
			"but found HostPath volume %s", volume.Name)})
	}
	if volume.CSI != nil && features.PodSpecVolumesCSI != config.Enabled {
		errs = errs.Also(&apis.FieldError{Message: fmt.Sprintf("CSI volume support is disabled, "+
			"but found CSI volume %s", volume.Name)})
	}
	errs = errs.Also(apis.CheckDisallowedFields(volume, *VolumeMask(ctx, &volume)))
	if volume.Name == "" {
		errs = apis.ErrMissingField("name")
	} else if len(validation.IsDNS1123Label(volume.Name)) != 0 {
		errs = apis.ErrInvalidValue(volume.Name, "name")
	}
	vs := volume.VolumeSource
	errs = errs.Also(apis.CheckDisallowedFields(vs, *VolumeSourceMask(ctx, &vs)))
	var specified []string
	if vs.Secret != nil {
		specified = append(specified, "secret")
		for i, item := range vs.Secret.Items {
			errs = errs.Also(validateKeyToPath(item).ViaFieldIndex("items", i))
		}
	}
	if vs.ConfigMap != nil {
		specified = append(specified, "configMap")
		for i, item := range vs.ConfigMap.Items {
			errs = errs.Also(validateKeyToPath(item).ViaFieldIndex("items", i))
		}
	}
	if vs.Projected != nil {
		specified = append(specified, "projected")
		for i, proj := range vs.Projected.Sources {
			errs = errs.Also(validateProjectedVolumeSource(proj).ViaFieldIndex("projected", i))
		}
	}
	if vs.EmptyDir != nil {
		specified = append(specified, "emptyDir")
		errs = errs.Also(validateEmptyDirFields(vs.EmptyDir).ViaField("emptyDir"))
	}

	if vs.PersistentVolumeClaim != nil {
		specified = append(specified, "persistentVolumeClaim")
	}

	if vs.HostPath != nil {
		specified = append(specified, "hostPath")
	}

	if vs.CSI != nil {
		specified = append(specified, "csi")
	}

	if vs.Image != nil {
		specified = append(specified, "image")
		errs = errs.Also(validateImageVolumeSource(vs.Image).ViaField("image"))
	}

	if len(specified) == 0 {
		fieldPaths := []string{"secret", "configMap", "projected"}
		cfg := config.FromContextOrDefaults(ctx)
		if cfg.Features.PodSpecVolumesEmptyDir == config.Enabled {
			fieldPaths = append(fieldPaths, "emptyDir")
		}
		if cfg.Features.PodSpecPersistentVolumeClaim == config.Enabled {
			fieldPaths = append(fieldPaths, "persistentVolumeClaim")
		}
		if cfg.Features.PodSpecVolumesHostPath == config.Enabled {
			fieldPaths = append(fieldPaths, "hostPath")
		}
		if cfg.Features.PodSpecVolumesCSI == config.Enabled {
			fieldPaths = append(fieldPaths, "csi")
		}
		if cfg.Features.PodSpecVolumesImage == config.Enabled {
			fieldPaths = append(fieldPaths, "image")
		}
		errs = errs.Also(apis.ErrMissingOneOf(fieldPaths...))
	} else if len(specified) > 1 {
		errs = errs.Also(apis.ErrMultipleOneOf(specified...))
	}

	return errs
}

func validateProjectedVolumeSource(vp corev1.VolumeProjection) *apis.FieldError {
	errs := apis.CheckDisallowedFields(vp, *VolumeProjectionMask(&vp))
	specified := make([]string, 0, 1) // Most of the time there will be a success with a single element.
	if vp.Secret != nil {
		specified = append(specified, "secret")
		errs = errs.Also(validateSecretProjection(vp.Secret).ViaField("secret"))
	}
	if vp.ConfigMap != nil {
		specified = append(specified, "configMap")
		errs = errs.Also(validateConfigMapProjection(vp.ConfigMap).ViaField("configMap"))
	}
	if vp.ServiceAccountToken != nil {
		specified = append(specified, "serviceAccountToken")
		errs = errs.Also(validateServiceAccountTokenProjection(vp.ServiceAccountToken).ViaField("serviceAccountToken"))
	}
	if vp.DownwardAPI != nil {
		specified = append(specified, "downwardAPI")
		errs = errs.Also(validateDownwardAPIProjection(vp.DownwardAPI).ViaField("downwardAPI"))
	}
	if len(specified) == 0 {
		errs = errs.Also(apis.ErrMissingOneOf("secret", "configMap", "serviceAccountToken", "downwardAPI"))
	} else if len(specified) > 1 {
		errs = errs.Also(apis.ErrMultipleOneOf(specified...))
	}
	return errs
}

func validateConfigMapProjection(cmp *corev1.ConfigMapProjection) *apis.FieldError {
	errs := apis.CheckDisallowedFields(*cmp, *ConfigMapProjectionMask(cmp)).
		Also(apis.CheckDisallowedFields(
			cmp.LocalObjectReference, *LocalObjectReferenceMask(&cmp.LocalObjectReference)))
	if cmp.Name == "" {
		errs = errs.Also(apis.ErrMissingField("name"))
	}
	for i, item := range cmp.Items {
		errs = errs.Also(validateKeyToPath(item).ViaFieldIndex("items", i))
	}
	return errs
}

func validateSecretProjection(sp *corev1.SecretProjection) *apis.FieldError {
	errs := apis.CheckDisallowedFields(*sp, *SecretProjectionMask(sp)).
		Also(apis.CheckDisallowedFields(
			sp.LocalObjectReference, *LocalObjectReferenceMask(&sp.LocalObjectReference)))
	if sp.Name == "" {
		errs = errs.Also(apis.ErrMissingField("name"))
	}
	for i, item := range sp.Items {
		errs = errs.Also(validateKeyToPath(item).ViaFieldIndex("items", i))
	}
	return errs
}

func validateServiceAccountTokenProjection(sp *corev1.ServiceAccountTokenProjection) *apis.FieldError {
	errs := apis.CheckDisallowedFields(*sp, *ServiceAccountTokenProjectionMask(sp))
	// Audience & ExpirationSeconds are optional
	if sp.Path == "" {
		errs = errs.Also(apis.ErrMissingField("path"))
	}
	return errs
}

func validateDownwardAPIProjection(dapi *corev1.DownwardAPIProjection) *apis.FieldError {
	errs := apis.CheckDisallowedFields(*dapi, *DownwardAPIProjectionMask(dapi))
	for i := range dapi.Items {
		errs = errs.Also(validateDownwardAPIVolumeFile(&dapi.Items[i]).ViaFieldIndex("items", i))
	}
	return errs
}

func validateDownwardAPIVolumeFile(vf *corev1.DownwardAPIVolumeFile) *apis.FieldError {
	errs := apis.CheckDisallowedFields(*vf, *DownwardAPIVolumeFileMask(vf))
	if vf.FieldRef == nil && vf.ResourceFieldRef == nil {
		errs = errs.Also(apis.ErrMissingOneOf("fieldRef", "resourceFieldRef"))
	}
	if vf.FieldRef != nil && vf.ResourceFieldRef != nil {
		errs = errs.Also(apis.ErrGeneric("Within a single item, cannot set both", "resourceFieldRef", "fieldRef"))
	}
	if vf.Path == "" {
		errs = errs.Also(apis.ErrMissingField("path"))
	}
	return errs
}

func validateKeyToPath(k2p corev1.KeyToPath) *apis.FieldError {
	errs := apis.CheckDisallowedFields(k2p, *KeyToPathMask(&k2p))
	if k2p.Key == "" {
		errs = errs.Also(apis.ErrMissingField("key"))
	}
	if k2p.Path == "" {
		errs = errs.Also(apis.ErrMissingField("path"))
	}
	return errs
}

func validateEmptyDirFields(dir *corev1.EmptyDirVolumeSource) *apis.FieldError {
	var errs *apis.FieldError
	if dir.Medium != "" && dir.Medium != "Memory" {
		errs = errs.Also(apis.ErrInvalidValue(dir.Medium, "medium"))
	}
	if dir.SizeLimit != nil {
		if dir.SizeLimit.Value() < 0 {
			errs = errs.Also(apis.ErrInvalidValue(dir.SizeLimit, "sizeLimit"))
		}
	}
	return errs
}

func validateEnvValueFrom(ctx context.Context, source *corev1.EnvVarSource) *apis.FieldError {
	if source == nil {
		return nil
	}
	features := config.FromContextOrDefaults(ctx).Features
	return apis.CheckDisallowedFields(*source, *EnvVarSourceMask(source, features.PodSpecFieldRef != config.Disabled))
}

func getReservedEnvVarsPerContainerType(ctx context.Context) sets.Set[string] {
	if IsInSidecarContainer(ctx) || IsInitContainer(ctx) {
		return reservedSidecarEnvVars
	}
	return reservedEnvVars
}

func validateEnvVar(ctx context.Context, env corev1.EnvVar) *apis.FieldError {
	errs := apis.CheckDisallowedFields(env, *EnvVarMask(&env))

	if env.Name == "" {
		errs = errs.Also(apis.ErrMissingField("name"))
	} else if getReservedEnvVarsPerContainerType(ctx).Has(env.Name) {
		errs = errs.Also(&apis.FieldError{
			Message: fmt.Sprintf("%q is a reserved environment variable", env.Name),
			Paths:   []string{"name"},
		})
	}

	return errs.Also(validateEnvValueFrom(ctx, env.ValueFrom).ViaField("valueFrom"))
}

func validateEnv(ctx context.Context, envVars []corev1.EnvVar) *apis.FieldError {
	var errs *apis.FieldError
	for i, env := range envVars {
		errs = errs.Also(validateEnvVar(ctx, env).ViaIndex(i))
	}
	return errs
}

func validateEnvFrom(envFromList []corev1.EnvFromSource) *apis.FieldError {
	var errs *apis.FieldError
	for i := range envFromList {
		envFrom := envFromList[i]
		errs = errs.Also(apis.CheckDisallowedFields(envFrom, *EnvFromSourceMask(&envFrom)).ViaIndex(i))

		cm := envFrom.ConfigMapRef
		sm := envFrom.SecretRef
		if sm != nil {
			errs = errs.Also(apis.CheckDisallowedFields(*sm, *SecretEnvSourceMask(sm)).ViaIndex(i))
			errs = errs.Also(apis.CheckDisallowedFields(
				sm.LocalObjectReference, *LocalObjectReferenceMask(&sm.LocalObjectReference))).ViaIndex(i).ViaField("secretRef")
		}

		if cm != nil {
			errs = errs.Also(apis.CheckDisallowedFields(*cm, *ConfigMapEnvSourceMask(cm)).ViaIndex(i))
			errs = errs.Also(apis.CheckDisallowedFields(
				cm.LocalObjectReference, *LocalObjectReferenceMask(&cm.LocalObjectReference))).ViaIndex(i).ViaField("configMapRef")
		}
		if cm != nil && sm != nil {
			errs = errs.Also(apis.ErrMultipleOneOf("configMapRef", "secretRef"))
		} else if cm == nil && sm == nil {
			errs = errs.Also(apis.ErrMissingOneOf("configMapRef", "secretRef"))
		}
	}
	return errs
}

// ValidatePodSpec validates the pod spec
func ValidatePodSpec(ctx context.Context, ps corev1.PodSpec) *apis.FieldError {
	// This is inlined, and so it makes for a less meaningful
	// error message.
	// if equality.Semantic.DeepEqual(ps, corev1.PodSpec{}) {
	// 	return apis.ErrMissingField(apis.CurrentField)
	// }

	errs := apis.CheckDisallowedFields(ps, *PodSpecMask(ctx, &ps))

	errs = errs.Also(ValidatePodSecurityContext(ctx, ps.SecurityContext).ViaField("securityContext"))

	for i := range ps.Containers {
		errs = errs.Also(
			warnDefaultContainerSecurityContext(ctx, ps.SecurityContext, ps.Containers[i].SecurityContext).
				ViaField("securityContext").ViaFieldIndex("containers", i))
	}
	for i := range ps.InitContainers {
		errs = errs.Also(
			warnDefaultContainerSecurityContext(ctx, ps.SecurityContext, ps.InitContainers[i].SecurityContext).
				ViaField("securityContext").ViaFieldIndex("initContainers", i))
	}

	volumes, err := ValidateVolumes(ctx, ps.Volumes, AllMountedVolumes(append(ps.InitContainers, ps.Containers...)))
	errs = errs.Also(err.ViaField("volumes"))

	errs = errs.Also(validateInitContainers(ctx, ps.InitContainers, ps.Containers, volumes))

	port, err := validateContainersPorts(ps.Containers)
	errs = errs.Also(err.ViaField("containers[*]"))

	switch len(ps.Containers) {
	case 0:
		errs = errs.Also(apis.ErrMissingField("containers"))
	case 1:
		errs = errs.Also(ValidateUserContainer(ctx, ps.Containers[0], volumes, port).
			ViaFieldIndex("containers", 0))
	default:
		errs = errs.Also(validateContainers(ctx, ps.Containers, volumes, port))
	}
	if ps.ServiceAccountName != "" {
		for _, err := range validation.IsDNS1123Subdomain(ps.ServiceAccountName) {
			errs = errs.Also(apis.ErrInvalidValue(ps.ServiceAccountName, "serviceAccountName", err))
		}
	}
	return errs
}

func validateInitContainers(ctx context.Context, containers, otherContainers []corev1.Container, volumes map[string]corev1.Volume) (errs *apis.FieldError) {
	if len(containers) == 0 {
		return nil
	}
	features := config.FromContextOrDefaults(ctx).Features
	if features.PodSpecInitContainers != config.Enabled {
		return errs.Also(&apis.FieldError{Message: fmt.Sprintf("pod spec support for init-containers is off, "+
			"but found %d init containers", len(containers))})
	}
	allNames := make(sets.Set[string], len(otherContainers)+len(containers))
	for _, ctr := range otherContainers {
		allNames.Insert(ctr.Name)
	}
	for i := range containers {
		if allNames.Has(containers[i].Name) {
			errs = errs.Also(&apis.FieldError{
				Message: fmt.Sprintf("duplicate container name %q", containers[i].Name),
				Paths:   []string{"name"},
			}).ViaFieldIndex("containers", i)
		}
		errs = errs.Also(validateInitContainer(ctx, containers[i], volumes).ViaFieldIndex("containers", i))
	}
	return errs
}

func validateContainers(ctx context.Context, containers []corev1.Container, volumes map[string]corev1.Volume, port corev1.ContainerPort) (errs *apis.FieldError) {
	features := config.FromContextOrDefaults(ctx).Features
	if features.MultiContainer != config.Enabled {
		return errs.Also(&apis.FieldError{Message: fmt.Sprintf("multi-container is off, "+
			"but found %d containers", len(containers))})
	}
	allNames := make(sets.Set[string], len(containers))
	for i := range containers {
		if allNames.Has(containers[i].Name) {
			errs = errs.Also(&apis.FieldError{
				Message: fmt.Sprintf("duplicate container name %q", containers[i].Name),
				Paths:   []string{"name"},
			}).ViaFieldIndex("containers", i)
		} else {
			allNames.Insert(containers[i].Name)
		}
		if len(containers[i].Ports) == 0 {
			errs = errs.Also(validateSidecarContainer(WithinSidecarContainer(ctx), containers[i], volumes).ViaFieldIndex("containers", i))
		} else {
			errs = errs.Also(ValidateUserContainer(WithinUserContainer(ctx), containers[i], volumes, port).ViaFieldIndex("containers", i))
		}
	}
	return errs
}

// AllMountedVolumes returns all the mounted volumes in all the containers.
func AllMountedVolumes(containers []corev1.Container) sets.Set[string] {
	volumeNames := sets.New[string]()
	for _, c := range containers {
		for _, vm := range c.VolumeMounts {
			volumeNames.Insert(vm.Name)
		}
	}
	return volumeNames
}

// validateContainersPorts validates that the expected number of container ports are present
// and returns the single serving port if error is nil.
func validateContainersPorts(containers []corev1.Container) (corev1.ContainerPort, *apis.FieldError) {
	var count int
	var port corev1.ContainerPort
	for i := range containers {
		if c := len(containers[i].Ports); c > 0 {
			count += c
			port = containers[i].Ports[0]
		}
	}

	if port.Name == "" {
		port.Name = "http"
	}

	if port.ContainerPort == 0 {
		port.ContainerPort = 8080
	}

	// When no container ports are specified.
	if count == 0 && len(containers) > 1 {
		return port, apis.ErrMissingField("ports")
	}

	// More than one container sections have ports.
	if count > 1 {
		return port, &apis.FieldError{
			Message: "more than one container port is set",
			Paths:   []string{"ports"},
			Details: "Only a single port is allowed across all containers",
		}
	}

	return port, nil
}

// validateSidecarContainer validate fields for non serving containers
func validateSidecarContainer(ctx context.Context, container corev1.Container, volumes map[string]corev1.Volume) (errs *apis.FieldError) {
	cfg := config.FromContextOrDefaults(ctx)
	if cfg.Features.MultiContainerProbing != config.Enabled {
		if container.LivenessProbe != nil {
			errs = errs.Also(apis.CheckDisallowedFields(*container.LivenessProbe,
				*ProbeMask(&corev1.Probe{})).ViaField("livenessProbe"))
		}
		if container.ReadinessProbe != nil {
			errs = errs.Also(apis.CheckDisallowedFields(*container.ReadinessProbe,
				*ProbeMask(&corev1.Probe{})).ViaField("readinessProbe"))
		}
	} else if cfg.Features.MultiContainerProbing == config.Enabled {
		// Liveness Probes
		errs = errs.Also(validateProbe(container.LivenessProbe, nil, false).ViaField("livenessProbe"))
		// Readiness Probes
		errs = errs.Also(validateReadinessProbe(container.ReadinessProbe, nil, false).ViaField("readinessProbe"))
	}

	return errs.Also(validate(ctx, container, volumes))
}

func validateInitContainer(ctx context.Context, container corev1.Container, volumes map[string]corev1.Volume) (errs *apis.FieldError) {
	// This is checked at the K8s side for init containers so better validate early if possible
	// ref: https://bit.ly/3tLZJV1
	if container.LivenessProbe != nil {
		errs = errs.Also(&apis.FieldError{
			Message: "field not allowed in an init container",
			Paths:   []string{"livenessProbe"},
		})
	}
	if container.ReadinessProbe != nil {
		errs = errs.Also(&apis.FieldError{
			Message: "field not allowed in an init container",
			Paths:   []string{"readinessProbe"},
		})
	}
	if container.StartupProbe != nil {
		errs = errs.Also(&apis.FieldError{
			Message: "field not allowed in an init container",
			Paths:   []string{"startupProbe"},
		})
	}
	if container.Lifecycle != nil {
		errs = errs.Also(&apis.FieldError{
			Message: "field not allowed in an init container",
			Paths:   []string{"lifecycle"},
		})
	}
	return errs.Also(validate(WithinInitContainer(ctx), container, volumes))
}

// ValidateUserContainer validate fields for serving containers
func ValidateUserContainer(ctx context.Context, container corev1.Container, volumes map[string]corev1.Volume, port corev1.ContainerPort) (errs *apis.FieldError) {
	// Liveness Probes
	errs = errs.Also(validateProbe(container.LivenessProbe, &port, true).ViaField("livenessProbe"))
	// Readiness Probes
	errs = errs.Also(validateReadinessProbe(container.ReadinessProbe, &port, true).ViaField("readinessProbe"))
	return errs.Also(validate(ctx, container, volumes))
}

func validate(ctx context.Context, container corev1.Container, volumes map[string]corev1.Volume) *apis.FieldError {
	if equality.Semantic.DeepEqual(container, corev1.Container{}) {
		return apis.ErrMissingField(apis.CurrentField)
	}

	errs := apis.CheckDisallowedFields(container, *ContainerMask(&container))

	if reservedContainerNames.Has(container.Name) {
		errs = errs.Also(&apis.FieldError{
			Message: fmt.Sprintf("%q is a reserved container name", container.Name),
			Paths:   []string{"name"},
		})
	}

	// Env
	errs = errs.Also(validateEnv(ctx, container.Env).ViaField("env"))
	// EnvFrom
	errs = errs.Also(validateEnvFrom(container.EnvFrom).ViaField("envFrom"))
	// Image
	if container.Image == "" {
		errs = errs.Also(apis.ErrMissingField("image"))
	} else if _, err := name.ParseReference(container.Image, name.WeakValidation); err != nil {
		fe := &apis.FieldError{
			Message: "Failed to parse image reference",
			Paths:   []string{"image"},
			Details: fmt.Sprintf("image: %q, error: %v", container.Image, err),
		}
		errs = errs.Also(fe)
	}
	// Ports
	if IsInitContainer(ctx) {
		errs = errs.Also(validateInitContainerPorts(container.Ports).ViaField("ports"))
	} else {
		errs = errs.Also(validateContainerPorts(container.Ports).ViaField("ports"))
	}
	// Resources
	errs = errs.Also(validateResources(&container.Resources).ViaField("resources"))
	// SecurityContext
	errs = errs.Also(validateSecurityContext(ctx, container.SecurityContext).ViaField("securityContext"))
	// TerminationMessagePolicy
	switch container.TerminationMessagePolicy {
	case corev1.TerminationMessageReadFile, corev1.TerminationMessageFallbackToLogsOnError, "":
	default:
		errs = errs.Also(apis.ErrInvalidValue(container.TerminationMessagePolicy, "terminationMessagePolicy"))
	}
	// VolumeMounts
	errs = errs.Also(validateVolumeMounts(ctx, container.VolumeMounts, volumes).ViaField("volumeMounts"))

	return errs
}

func validateResources(resources *corev1.ResourceRequirements) *apis.FieldError {
	if resources == nil {
		return nil
	}
	return apis.CheckDisallowedFields(*resources, *ResourceRequirementsMask(resources))
}

func validateCapabilities(ctx context.Context, cap *corev1.Capabilities) *apis.FieldError {
	if cap == nil {
		return nil
	}
	return apis.CheckDisallowedFields(*cap, *CapabilitiesMask(ctx, cap))
}

func validateSecurityContext(ctx context.Context, sc *corev1.SecurityContext) *apis.FieldError {
	if sc == nil {
		return nil
	}
	errs := apis.CheckDisallowedFields(*sc, *SecurityContextMask(ctx, sc))

	errs = errs.Also(validateCapabilities(ctx, sc.Capabilities).ViaField("capabilities"))

	if sc.RunAsUser != nil {
		uid := *sc.RunAsUser
		if uid < minUserID || uid > maxUserID {
			errs = errs.Also(apis.ErrOutOfBoundsValue(uid, minUserID, maxUserID, "runAsUser"))
		}
	}

	if sc.RunAsGroup != nil {
		gid := *sc.RunAsGroup
		if gid < minGroupID || gid > maxGroupID {
			errs = errs.Also(apis.ErrOutOfBoundsValue(gid, minGroupID, maxGroupID, "runAsGroup"))
		}
	}
	return errs
}

func validateVolumeMounts(ctx context.Context, mounts []corev1.VolumeMount, volumes map[string]corev1.Volume) *apis.FieldError {
	var errs *apis.FieldError
	// Check that volume mounts match names in "volumes", that "volumes" has 100%
	// coverage, and the field restrictions.
	features := config.FromContextOrDefaults(ctx).Features
	seenName := make(sets.Set[string], len(mounts))
	seenMountPath := make(sets.Set[string], len(mounts))
	for i := range mounts {
		vm := mounts[i]
		errs = errs.Also(apis.CheckDisallowedFields(vm, *VolumeMountMask(ctx, &vm)).ViaIndex(i))
		// This effectively checks that Name is non-empty because Volume name must be non-empty.
		if _, ok := volumes[vm.Name]; !ok {
			errs = errs.Also((&apis.FieldError{
				Message: "volumeMount has no matching volume",
				Paths:   []string{"name"},
			}).ViaIndex(i))
		}
		seenName.Insert(vm.Name)

		if vm.MountPath == "" {
			errs = errs.Also(apis.ErrMissingField("mountPath").ViaIndex(i))
		} else if reservedPaths.Has(path.Clean(vm.MountPath)) {
			errs = errs.Also((&apis.FieldError{
				Message: fmt.Sprintf("mountPath %q is a reserved path", path.Clean(vm.MountPath)),
				Paths:   []string{"mountPath"},
			}).ViaIndex(i))
		} else if !path.IsAbs(vm.MountPath) {
			errs = errs.Also(apis.ErrInvalidValue(vm.MountPath, "mountPath").ViaIndex(i))
		} else if seenMountPath.Has(path.Clean(vm.MountPath)) {
			errs = errs.Also(apis.ErrInvalidValue(
				fmt.Sprintf("%q must be unique", vm.MountPath), "mountPath").ViaIndex(i))
		}
		seenMountPath.Insert(path.Clean(vm.MountPath))

		shouldCheckReadOnlyVolume := volumes[vm.Name].EmptyDir == nil && volumes[vm.Name].PersistentVolumeClaim == nil
		if shouldCheckReadOnlyVolume && !vm.ReadOnly {
			errs = errs.Also((&apis.FieldError{
				Message: "volume mount should be readOnly for this type of volume",
				Paths:   []string{"readOnly"},
			}).ViaIndex(i))
		}
		if vm.MountPropagation != nil {
			if features.PodSpecVolumesMountPropagation != config.Enabled {
				errs = errs.Also((&apis.FieldError{
					Message: fmt.Sprintf("Volume Mount Propagation support is disabled, but found volume mount %s with mount propagation", vm.Name),
				}).ViaIndex(i))
			}
			if *vm.MountPropagation != corev1.MountPropagationNone && *vm.MountPropagation != corev1.MountPropagationHostToContainer {
				errs = errs.Also((&apis.FieldError{
					Message: "mount propagation should be set to None or HostToContainer",
					Paths:   []string{"mountPropagation"},
				}).ViaIndex(i))
			}
		}

		if volumes[vm.Name].PersistentVolumeClaim != nil {
			if volumes[vm.Name].PersistentVolumeClaim.ReadOnly && !vm.ReadOnly {
				errs = errs.Also((&apis.FieldError{
					Message: "volume is readOnly but volume mount is not",
					Paths:   []string{"readOnly"},
				}).ViaIndex(i))
			}
		}
	}
	return errs
}

func validateContainerPorts(ports []corev1.ContainerPort) *apis.FieldError {
	if len(ports) == 0 {
		return nil
	}

	var errs *apis.FieldError

	// user can set container port which names "user-port" to define application's port.
	// Queue-proxy will use it to send requests to application
	// if user didn't set any port, it will set default port user-port=8080.
	userPort := ports[0]

	errs = errs.Also(validateContainerPortBasic(userPort))

	// Only allow empty (defaulting to "TCP") or explicit TCP for protocol
	if userPort.Protocol != "" && userPort.Protocol != corev1.ProtocolTCP {
		errs = errs.Also(apis.ErrInvalidValue(userPort.Protocol, "protocol"))
	}

	if !validPortNames.Has(userPort.Name) {
		errs = errs.Also(&apis.FieldError{
			Message: fmt.Sprintf("Port name %v is not allowed", ports[0].Name),
			Paths:   []string{apis.CurrentField},
			Details: "Name must be empty, or one of: 'h2c', 'http1'",
		})
	}

	return errs
}

func validateInitContainerPorts(ports []corev1.ContainerPort) *apis.FieldError {
	var errs *apis.FieldError
	if len(ports) == 0 {
		return nil
	}
	for _, p := range ports {
		errs = errs.Also(validateContainerPortBasic(p))
	}
	return errs
}

func validateContainerPortBasic(port corev1.ContainerPort) *apis.FieldError {
	var errs *apis.FieldError
	errs = errs.Also(apis.CheckDisallowedFields(port, *ContainerPortMask(&port)))

	if reservedPorts.Has(port.ContainerPort) {
		errs = errs.Also(apis.ErrInvalidValue(fmt.Sprintf("%d is a reserved port", port.ContainerPort), "containerPort",
			fmt.Sprintf("%d is a reserved port, please use a different value", port.ContainerPort)))
	}

	if port.ContainerPort < 0 || port.ContainerPort > 65535 {
		errs = errs.Also(apis.ErrOutOfBoundsValue(port.ContainerPort, 0, 65535, "containerPort"))
	}
	return errs
}

func validateReadinessProbe(p *corev1.Probe, port *corev1.ContainerPort, isUserContainer bool) *apis.FieldError {
	if p == nil {
		return nil
	}

	errs := validateProbe(p, port, isUserContainer)

	if p.PeriodSeconds < 0 {
		errs = errs.Also(apis.ErrOutOfBoundsValue(p.PeriodSeconds, 0, math.MaxInt32, "periodSeconds"))
	}

	if p.InitialDelaySeconds < 0 {
		errs = errs.Also(apis.ErrOutOfBoundsValue(p.InitialDelaySeconds, 0, math.MaxInt32, "initialDelaySeconds"))
	}

	if p.SuccessThreshold < 1 {
		errs = errs.Also(apis.ErrOutOfBoundsValue(p.SuccessThreshold, 1, math.MaxInt32, "successThreshold"))
	}

	// PeriodSeconds == 0 indicates Knative's special probe with aggressive retries
	if p.PeriodSeconds == 0 {
		if p.FailureThreshold != 0 {
			errs = errs.Also(&apis.FieldError{
				Message: "failureThreshold is disallowed when periodSeconds is zero",
				Paths:   []string{"failureThreshold"},
			})
		}

		if p.TimeoutSeconds != 0 {
			errs = errs.Also(&apis.FieldError{
				Message: "timeoutSeconds is disallowed when periodSeconds is zero",
				Paths:   []string{"timeoutSeconds"},
			})
		}
	} else {
		if p.TimeoutSeconds < 1 {
			errs = errs.Also(apis.ErrOutOfBoundsValue(p.TimeoutSeconds, 1, math.MaxInt32, "timeoutSeconds"))
		}

		if p.FailureThreshold < 1 {
			errs = errs.Also(apis.ErrOutOfBoundsValue(p.FailureThreshold, 1, math.MaxInt32, "failureThreshold"))
		}
	}

	return errs
}

func validateProbe(p *corev1.Probe, port *corev1.ContainerPort, isUserContainer bool) *apis.FieldError {
	if p == nil {
		return nil
	}
	errs := apis.CheckDisallowedFields(*p, *ProbeMask(p))

	h := p.ProbeHandler
	errs = errs.Also(apis.CheckDisallowedFields(h, *HandlerMask(&h)))

	var handlers []string

	if h.HTTPGet != nil {
		handlers = append(handlers, "httpGet")
		errs = errs.Also(apis.CheckDisallowedFields(*h.HTTPGet, *HTTPGetActionMask(h.HTTPGet))).ViaField("httpGet")
		getPort := h.HTTPGet.Port
		if isUserContainer {
			if getPort.StrVal != "" && getPort.StrVal != port.Name {
				errs = errs.Also(apis.ErrInvalidValue(getPort.String(), "httpGet.port", "Probe port must match container port"))
			}
		} else {
			if getPort.StrVal == "" && getPort.IntVal == 0 {
				errs = errs.Also(apis.ErrInvalidValue(getPort.String(), "httpGet.port", "Probe port must be specified"))
			}
		}
	}
	if h.TCPSocket != nil {
		handlers = append(handlers, "tcpSocket")
		errs = errs.Also(apis.CheckDisallowedFields(*h.TCPSocket, *TCPSocketActionMask(h.TCPSocket))).ViaField("tcpSocket")
		tcpPort := h.TCPSocket.Port
		if isUserContainer {
			if tcpPort.StrVal != "" && tcpPort.StrVal != port.Name {
				errs = errs.Also(apis.ErrInvalidValue(tcpPort.String(), "tcpSocket.port", "Probe port must match container port"))
			}
		} else {
			if tcpPort.StrVal == "" && tcpPort.IntVal == 0 {
				errs = errs.Also(apis.ErrInvalidValue(tcpPort.String(), "tcpSocket.port", "Probe port must be specified"))
			}
		}
	}
	if h.Exec != nil {
		handlers = append(handlers, "exec")
		errs = errs.Also(apis.CheckDisallowedFields(*h.Exec, *ExecActionMask(h.Exec))).ViaField("exec")
	}
	if h.GRPC != nil {
		handlers = append(handlers, "gRPC")
		errs = errs.Also(apis.CheckDisallowedFields(*h.GRPC, *GRPCActionMask(h.GRPC))).ViaField("grpc")
	}

	if len(handlers) == 0 {
		errs = errs.Also(apis.ErrMissingOneOf("httpGet", "tcpSocket", "exec", "grpc"))
	} else if len(handlers) > 1 {
		errs = errs.Also(apis.ErrMultipleOneOf(handlers...))
	}
	return errs
}

// ValidateNamespacedObjectReference validates an ObjectReference which may not contain a namespace.
func ValidateNamespacedObjectReference(p *corev1.ObjectReference) *apis.FieldError {
	if p == nil {
		return nil
	}
	errs := apis.CheckDisallowedFields(*p, *NamespacedObjectReferenceMask(p))

	if p.APIVersion == "" {
		errs = errs.Also(apis.ErrMissingField("apiVersion"))
	} else if verrs := validation.IsQualifiedName(p.APIVersion); len(verrs) != 0 {
		errs = errs.Also(apis.ErrInvalidValue(strings.Join(verrs, ", "), "apiVersion"))
	}
	if p.Kind == "" {
		errs = errs.Also(apis.ErrMissingField("kind"))
	} else if verrs := validation.IsCIdentifier(p.Kind); len(verrs) != 0 {
		errs = errs.Also(apis.ErrInvalidValue(strings.Join(verrs, ", "), "kind"))
	}
	if p.Name == "" {
		errs = errs.Also(apis.ErrMissingField("name"))
	} else if verrs := validation.IsDNS1123Label(p.Name); len(verrs) != 0 {
		errs = errs.Also(apis.ErrInvalidValue(strings.Join(verrs, ", "), "name"))
	}
	return errs
}

// ValidatePodSecurityContext validates the PodSecurityContext struct. All fields are disallowed
// unless the 'PodSpecSecurityContext' feature flag is enabled
//
// See the allowed properties in the `PodSecurityContextMask`
func ValidatePodSecurityContext(ctx context.Context, sc *corev1.PodSecurityContext) *apis.FieldError {
	if sc == nil {
		return nil
	}

	errs := apis.CheckDisallowedFields(*sc, *PodSecurityContextMask(ctx, sc))

	if sc.RunAsUser != nil {
		uid := *sc.RunAsUser
		if uid < minUserID || uid > maxUserID {
			errs = errs.Also(apis.ErrOutOfBoundsValue(uid, minUserID, maxUserID, "runAsUser"))
		}
	}

	if sc.RunAsGroup != nil {
		gid := *sc.RunAsGroup
		if gid < minGroupID || gid > maxGroupID {
			errs = errs.Also(apis.ErrOutOfBoundsValue(gid, minGroupID, maxGroupID, "runAsGroup"))
		}
	}

	if sc.FSGroup != nil {
		gid := *sc.FSGroup
		if gid < minGroupID || gid > maxGroupID {
			errs = errs.Also(apis.ErrOutOfBoundsValue(gid, minGroupID, maxGroupID, "fsGroup"))
		}
	}

	for i, gid := range sc.SupplementalGroups {
		if gid < minGroupID || gid > maxGroupID {
			err := apis.ErrOutOfBoundsValue(gid, minGroupID, maxGroupID, "").
				ViaFieldIndex("supplementalGroups", i)
			errs = errs.Also(err)
		}
	}

	return errs
}

// warnDefaultContainerSecurityContext warns about Kubernetes default
// SecurityContext values which are unset and thus insecure (i.e. the
// "restricted" profile forbids these values). Because securityContext values
// may also be set at the Pod level, the container-level settings need to be
// considered alongside the Pod-level settings.
//
// Note that this **explicitly** does not warn on dangerous SecurityContext
// settings, the purpose is to avoid accidentally-insecure settings, not to
// block deliberate use of dangerous settings.
func warnDefaultContainerSecurityContext(_ context.Context, psc *corev1.PodSecurityContext, sc *corev1.SecurityContext) *apis.FieldError {
	if sc == nil {
		sc = &corev1.SecurityContext{}
	}
	if psc == nil {
		psc = &corev1.PodSecurityContext{}
	}

	insecureDefault := func(fieldPath string) *apis.FieldError {
		return apis.ErrGeneric("Kubernetes default value is insecure, Knative may default this to secure in a future release", fieldPath).At(apis.WarningLevel)
	}

	var errs *apis.FieldError
	if psc.RunAsNonRoot == nil && sc.RunAsNonRoot == nil {
		errs = errs.Also(insecureDefault("runAsNonRoot"))
	}

	if sc.AllowPrivilegeEscalation == nil {
		errs = errs.Also(insecureDefault("allowPrivilegeEscalation"))
	}

	if sc.SeccompProfile == nil && psc.SeccompProfile == nil {
		errs = errs.Also(insecureDefault("seccompProfile"))
	} else {
		pscIsDefault := psc.SeccompProfile == nil || psc.SeccompProfile.Type == ""
		scIsDefault := sc.SeccompProfile == nil || sc.SeccompProfile.Type == ""
		if pscIsDefault && scIsDefault {
			errs = errs.Also(insecureDefault("seccompProfile.type"))
		}
	}

	if sc.Capabilities == nil {
		errs = errs.Also(insecureDefault("capabilities"))
	} else {
		if sc.Capabilities.Drop == nil {
			errs = errs.Also(insecureDefault("capabilities.drop"))
		} else if len(sc.Capabilities.Drop) > 0 && sc.Capabilities.Drop[0] == "all" {
			// Sometimes, people mis-spell "ALL" as "all", which does nothing.
			errs = errs.Also(apis.ErrInvalidValue("all", "capabilities.drop", "Must be spelled as 'ALL'").At(apis.WarningLevel))
		}
	}
	return errs
}

// This is attached to contexts as they are passed down through a user container
// being validated.
type userContainer struct{}

// WithinUserContainer notes on the context that further validation or defaulting
// is within the context of a user container in the revision.
func WithinUserContainer(ctx context.Context) context.Context {
	return context.WithValue(ctx, userContainer{}, struct{}{})
}

// This is attached to contexts as they are passed down through a sidecar container
// being validated.
type sidecarContainer struct{}

// WithinSidecarContainer notes on the context that further validation or defaulting
// is within the context of a sidecar container in the revision.
func WithinSidecarContainer(ctx context.Context) context.Context {
	return context.WithValue(ctx, sidecarContainer{}, struct{}{})
}

// IsInSidecarContainer checks if we are in the context of a sidecar container in the revision.
func IsInSidecarContainer(ctx context.Context) bool {
	return ctx.Value(sidecarContainer{}) != nil
}

// This is attached to contexts as they are passed down through an init container
// being validated.
type initContainer struct{}

// WithinInitContainer notes on the context that further validation or defaulting
// is within the context of an init container in the revision.
func WithinInitContainer(ctx context.Context) context.Context {
	return context.WithValue(ctx, initContainer{}, struct{}{})
}

// IsInitContainer checks if we are in the context of an init container in the revision.
func IsInitContainer(ctx context.Context) bool {
	return ctx.Value(initContainer{}) != nil
}

func validateImageVolumeSource(iv *corev1.ImageVolumeSource) *apis.FieldError {
	errs := apis.CheckDisallowedFields(*iv, *ImageVolumeSourceMask(iv))

	if iv.Reference == "" {
		errs = errs.Also(apis.ErrMissingField("reference"))
	}

	switch iv.PullPolicy {
	case corev1.PullIfNotPresent, corev1.PullAlways, corev1.PullNever, "":
		// ok
	default:
		errs = errs.Also(apis.ErrInvalidValue(iv.PullPolicy, "pullPolicy"))
	}

	return errs
}
