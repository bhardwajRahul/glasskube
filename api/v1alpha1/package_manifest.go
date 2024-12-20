/*
Copyright 2024.

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

package v1alpha1

import (
	"strings"

	"github.com/invopop/jsonschema"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

type HelmRelease struct {
	// ChartName is the name of the chart that represents this package.
	ChartName string `json:"chartName" jsonschema:"required"`
	// ChartVersion of the chart that should be installed.
	ChartVersion string `json:"chartVersion" jsonschema:"required"`
	// Values that should be used for the helm release
	Values *JSON `json:"values,omitempty"`
}

// +kubebuilder:validation:XValidation:message="",rule="size(self.releases) > 0 || (has(self.chartName) && has(self.chartVersion))"
type HelmManifest struct {
	// RepositoryUrl is the remote URL of the helm repository. This is the same URL you would use
	// if you use "helm repo add ...".
	RepositoryUrl string        `json:"repositoryUrl" jsonschema:"required"`
	Releases      []HelmRelease `json:"releases,omitempty" jsonschema:"oneof_required=MultiChart"`

	// ChartName is the name of the chart that represents this package.
	ChartName string `json:"chartName,omitempty" jsonschema:"oneof_required=SingleChart"`
	// ChartVersion of the chart that should be installed.
	ChartVersion string `json:"chartVersion,omitempty" jsonschema:"oneof_required=SingleChart"`
	// Values that should be used for the helm release
	Values *JSON `json:"values,omitempty"`
}

func (obj HelmManifest) IsOCIRepository() bool {
	return strings.HasPrefix(obj.RepositoryUrl, "oci://")
}

type KustomizeManifest struct {
}

// PackageEntrypoint defines a service port a user may use to access the package
type PackageEntrypoint struct {
	// Name of this entrypoint. Used for "glasskube open [package-name] [entrypoint-name]" if more
	// than one entrypoint exists. Optional if the package only has one entrypoint.
	Name string `json:"name,omitempty"`
	// ServiceName is the name of a service that is part of
	ServiceName string `json:"serviceName" jsonschema:"required"`
	// Port of the service to bind to
	Port int32 `json:"port" jsonschema:"required"`
	// LocalPort to use for port mapping
	LocalPort int32  `json:"localPort,omitempty"`
	Scheme    string `json:"scheme,omitempty"`
}

type PlainManifest struct {
	// Url is the location of the manifest.
	// Typically, this should be a full https URL, but local paths are also supporeted.
	// If this field is set to a local path (e.g. a relative path like "./manifest.yaml" or just "manifest.yaml") it
	// will be resolved relative to the packages "package.yaml" file.
	Url string `json:"url" jsonschema:"required"`
	// DefaultNamespace, if set to a non-empty string, is used for resources that are of a namespaced
	// kind and do not have a namespace set.
	// If at least one such a resource exists, the namespace is created implicitly.
	DefaultNamespace string `json:"defaultNamespace,omitempty"`
}

type PackageReference struct {
	Label string `json:"label" jsonschema:"required"`
	Url   string `json:"url" jsonschema:"required"`
}

type Dependency struct {
	Name    string `json:"name" jsonschema:"required"`
	Version string `json:"version,omitempty"`
}

type Component struct {
	Name          string `json:"name" jsonschema:"required"`
	InstalledName string `json:"installedName,omitempty"`
	Version       string `json:"version,omitempty"`
	// Specify configuration for this component
	Values ComponentValues `json:"values,omitempty"`
}

type ComponentValues map[string]InlineValueConfiguration

func (values ComponentValues) AsPackageValues() map[string]ValueConfiguration {
	result := make(map[string]ValueConfiguration, len(values))
	for name, value := range values {
		result[name] = ValueConfiguration{InlineValueConfiguration: value}
	}
	return result
}

// +kubebuilder:validation:Enum=Cluster;Namespaced
type PackageScope string

const (
	ScopeCluster    PackageScope = PackageScope(apiextv1.ClusterScoped)
	ScopeNamespaced PackageScope = PackageScope(apiextv1.NamespaceScoped)
)

func (PackageScope) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Enum: []any{
			ScopeCluster,
			ScopeNamespaced,
		},
	}
}

func (s *PackageScope) IsCluster() bool {
	return s == nil || *s == ScopeCluster
}

func (s *PackageScope) IsNamespaced() bool {
	return s != nil && *s == ScopeNamespaced
}

type TransformationSource struct {
	Resource *corev1.TypedLocalObjectReference `json:"resource,omitempty"`
	Path     string                            `json:"path" jsonschema:"required"`
}

type TransformationDefinition struct {
	Source  TransformationSource    `json:"source" jsonschema:"required"`
	Targets []ValueDefinitionTarget `json:"targets" jsonschema:"required"`
}

type PackageManifest struct {
	// Scope is optional (default is Cluster)
	Scope            *PackageScope      `json:"scope,omitempty"`
	Name             string             `json:"name" jsonschema:"required"`
	ShortDescription string             `json:"shortDescription,omitempty"`
	LongDescription  string             `json:"longDescription,omitempty"`
	References       []PackageReference `json:"references,omitempty"`
	IconUrl          string             `json:"iconUrl,omitempty" jsonschema:"format=uri"`
	// Helm instructs the controller to create a helm release when installing this package.
	Helm *HelmManifest `json:"helm,omitempty"`
	// Kustomize instructs the controller to apply a kustomization when installing this package [PLACEHOLDER].
	Kustomize           *KustomizeManifest                 `json:"kustomize,omitempty"`
	Manifests           []PlainManifest                    `json:"manifests,omitempty"`
	ValueDefinitions    map[string]ValueDefinition         `json:"valueDefinitions,omitempty"`
	Transformations     []TransformationDefinition         `json:"transformations,omitempty"`
	TransitiveResources []corev1.TypedLocalObjectReference `json:"transitiveResources,omitempty"`
	// DefaultNamespace to install the package. May be overridden.
	DefaultNamespace string              `json:"defaultNamespace,omitempty" jsonschema:"required"`
	Entrypoints      []PackageEntrypoint `json:"entrypoints,omitempty"`
	Dependencies     []Dependency        `json:"dependencies,omitempty"`
	Components       []Component         `json:"components,omitempty"`
}
