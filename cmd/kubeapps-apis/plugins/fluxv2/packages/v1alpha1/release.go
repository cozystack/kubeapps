package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/fluxv2/packages/v1alpha1/common"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/connecterror"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/pkgutils"
	"sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	log "k8s.io/klog/v2"
)

// getAvailableResourceTypes returns a list of available custom resource types
func (s *Server) getAvailableResourceTypes() []schema.GroupVersionResource {
	var gvrs []schema.GroupVersionResource
	for _, r := range s.pluginConfig.Resources {
		gvrs = append(gvrs, schema.GroupVersionResource{
			Group:    "apps.cozystack.io",
			Version:  "v1alpha1",
			Resource: r.Application.Plural,
		})
	}
	return gvrs
}

// listReleasesInCluster returns all installed applications
func (s *Server) listReleasesInCluster(ctx context.Context, headers http.Header, namespace string) ([]*unstructured.Unstructured, error) {
	var allReleases []*unstructured.Unstructured

	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvrs := s.getAvailableResourceTypes()

	for _, gvr := range gvrs {
		list, err := dynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Errorf("Failed to list resources for %s: %v", gvr.String(), err)
			continue
		}
		for i := range list.Items {
			allReleases = append(allReleases, &list.Items[i])
		}
	}

	return allReleases, nil
}

// getReleaseInCluster gets a specific application instance
func (s *Server) getReleaseInCluster(ctx context.Context, headers http.Header, gvr schema.GroupVersionResource, key types.NamespacedName) (*unstructured.Unstructured, error) {
	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	obj, err := dynamicClient.Resource(gvr).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("get", gvr.Resource, key.String(), err)
	}
	return obj, nil
}

func (s *Server) paginatedInstalledPkgSummaries(ctx context.Context, headers http.Header, namespace string, pageSize int32, offset int) ([]*corev1.InstalledPackageSummary, error) {
	releasesFromCluster, err := s.listReleasesInCluster(ctx, headers, namespace)
	if err != nil {
		return nil, err
	}

	summaries := []*corev1.InstalledPackageSummary{}
	if len(releasesFromCluster) > 0 {
		startAt := -1
		if pageSize > 0 {
			startAt = offset
		}

		for i, r := range releasesFromCluster {
			if startAt <= i {
				summary, err := s.installedPkgSummaryFromRelease(ctx, headers, r)
				if err != nil {
					return nil, err
				}
				if summary != nil {
					summaries = append(summaries, summary)
					if pageSize > 0 && len(summaries) == int(pageSize) {
						break
					}
				}
			}
		}
	}
	return summaries, nil
}

func (s *Server) installedPkgSummaryFromRelease(ctx context.Context, headers http.Header, rel *unstructured.Unstructured) (*corev1.InstalledPackageSummary, error) {
	name := rel.GetName()
	namespace := rel.GetNamespace()
	kind := rel.GetKind()
	version, _, _ := unstructured.NestedString(rel.Object, "appVersion")

	// Find the resource config
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Kind == kind {
			resourceConfig = &res
			break
		}
	}

	if resourceConfig == nil {
		return nil, fmt.Errorf("Resource config not found for kind: %s", kind)
	}

	// Create reference with kind included in identifier
	availablePkgRef := &corev1.AvailablePackageReference{
		Context: &corev1.Context{
			Namespace: resourceConfig.Release.Chart.SourceRef.Namespace,
			Cluster:   s.kubeappsCluster,
		},
		Identifier: fmt.Sprintf("%s/%s",
			resourceConfig.Release.Chart.SourceRef.Name,
			resourceConfig.Release.Chart.Name),
		Plugin: GetPluginDetail(),
	}

	// Get package details
	pkgDetail, err := s.getAvailablePackageDetail(ctx, headers, availablePkgRef)
	if err != nil {
		return nil, err
	}

	// Create identifier using ~ as separator
	identifier := fmt.Sprintf("%s~%s", kind, name)

	return &corev1.InstalledPackageSummary{
		InstalledPackageRef: &corev1.InstalledPackageReference{
			Context: &corev1.Context{
				Namespace: namespace,
				Cluster:   s.kubeappsCluster,
			},
			Identifier: identifier,
			Plugin:     GetPluginDetail(),
		},
		Name: name,
		CurrentVersion: &corev1.PackageAppVersion{
			PkgVersion: version,
			AppVersion: version,
		},
		IconUrl:          pkgDetail.IconUrl,
		PkgDisplayName:   pkgDetail.DisplayName,
		ShortDescription: pkgDetail.ShortDescription,
		Status:           getResourceStatus(rel),
	}, nil
}

func (s *Server) getAvailablePackageDetail(ctx context.Context, headers http.Header, ref *corev1.AvailablePackageReference) (*corev1.AvailablePackageDetail, error) {
	repoName, chartName, err := pkgutils.SplitPackageIdentifier(ref.Identifier)
	if err != nil {
		return nil, err
	}

	repo := types.NamespacedName{Namespace: ref.Context.Namespace, Name: repoName}
	chart, err := s.getChartModel(ctx, headers, repo, chartName)
	if err != nil {
		return nil, err
	}

	if chart == nil {
		return nil, fmt.Errorf("chart not found")
	}

	return &corev1.AvailablePackageDetail{
		Name:             chartName,
		IconUrl:          chart.Icon,
		DisplayName:      chart.Name,
		ShortDescription: chart.Description,
		Version: &corev1.PackageAppVersion{
			PkgVersion: chart.ChartVersions[0].Version,    // TODO
			AppVersion: chart.ChartVersions[0].AppVersion, // TODO
		},
	}, nil
}

func getResourceStatus(obj *unstructured.Unstructured) *corev1.InstalledPackageStatus {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")

	for _, c := range conditions {
		condition, ok := c.(map[string]interface{})
		if !ok {
			continue
		}

		if status, ok := condition["status"].(string); ok {
			if status == "True" {
				return &corev1.InstalledPackageStatus{
					Ready:      true,
					Reason:     corev1.InstalledPackageStatus_STATUS_REASON_INSTALLED,
					UserReason: fmt.Sprintf("%s is ready", obj.GetKind()),
				}
			}
		}
	}

	return &corev1.InstalledPackageStatus{
		Ready:      false,
		Reason:     corev1.InstalledPackageStatus_STATUS_REASON_PENDING,
		UserReason: fmt.Sprintf("%s is not ready", obj.GetKind()),
	}
}

// newRelease creates a new custom resource instance
func (s *Server) newRelease(ctx context.Context, headers http.Header, packageRef *corev1.AvailablePackageReference, targetName types.NamespacedName, version *corev1.VersionReference, values string) (*corev1.InstalledPackageReference, error) {
	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr, err := s.getGVRFromPackageID(packageRef.Identifier)
	if err != nil {
		return nil, err
	}

	// Find the resource config
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Plural == gvr.Resource {
			resourceConfig = &res
			break
		}
	}
	if resourceConfig == nil {
		return nil, fmt.Errorf("resource config not found for %s", gvr.Resource)
	}

	// Create the custom resource
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", gvr.Group, gvr.Version),
			"kind":       resourceConfig.Application.Kind,
			"metadata": map[string]interface{}{
				"name":      targetName.Name,
				"namespace": targetName.Namespace,
			},
		},
	}

	// Parse and set values in spec
	if values != "" {
		var specValues map[string]interface{}
		// Remove comments before parsing
		noComments := removeYAMLComments(values)
		if err := json.Unmarshal([]byte(noComments), &specValues); err != nil {
			// Try parsing as YAML if JSON fails
			if err2 := yaml.Unmarshal([]byte(noComments), &specValues); err2 != nil {
				return nil, fmt.Errorf("failed to parse values: %w", err)
			}
		}
		obj.Object["spec"] = specValues
	}

	// Set version if provided
	if version != nil && version.Version != "" {
		obj.Object["appVersion"] = version.Version
	}

	// Use the user's dynamic client to create the resource
	created, err := dynamicClient.Resource(gvr).Namespace(targetName.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("create", gvr.Resource, targetName.String(), err)
	}

	return &corev1.InstalledPackageReference{
		Context: &corev1.Context{
			Namespace: created.GetNamespace(),
			Cluster:   s.kubeappsCluster,
		},
		Identifier: fmt.Sprintf("%s/%s", resourceConfig.Application.Kind, created.GetName()),
		Plugin:     GetPluginDetail(),
	}, nil
}

// Helper function to remove YAML comments
func removeYAMLComments(input string) string {
	var result strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(input))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "#"); idx != -1 {
			line = strings.TrimSpace(line[:idx])
		}
		if line != "" {
			result.WriteString(line)
			result.WriteString("\n")
		}
	}
	return result.String()
}

func (s *Server) getGVRFromPackageID(packageID string) (schema.GroupVersionResource, error) {
	parts := strings.Split(packageID, "/")
	if len(parts) != 2 {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid package ID format: %s", packageID)
	}
	repoName := parts[0]
	chartName := parts[1]

	// Find matching resource from config
	for _, res := range s.pluginConfig.Resources {
		if res.Release.Chart.SourceRef.Name == repoName &&
			res.Release.Chart.Name == chartName {
			return schema.GroupVersionResource{
				Group:    "apps.cozystack.io",
				Version:  "v1alpha1",
				Resource: res.Application.Plural,
			}, nil
		}
	}

	return schema.GroupVersionResource{}, fmt.Errorf("no matching resource type found for package %s/%s", repoName, chartName)
}

// updateRelease updates an existing custom resource instance
func (s *Server) updateRelease(ctx context.Context, headers http.Header, packageRef *corev1.InstalledPackageReference, version *corev1.VersionReference, values string) (*corev1.InstalledPackageReference, error) {
	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr, err := s.getGVRFromPackageID(packageRef.Identifier)
	if err != nil {
		return nil, err
	}

	existing, err := dynamicClient.Resource(gvr).Namespace(packageRef.Context.Namespace).Get(ctx, packageRef.Identifier, metav1.GetOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("get", gvr.Resource, packageRef.Identifier, err)
	}

	if values != "" {
		var specValues map[string]interface{}
		if err := json.Unmarshal([]byte(values), &specValues); err != nil {
			return nil, fmt.Errorf("failed to parse values: %w", err)
		}
		existing.Object["spec"] = specValues
	}

	if version != nil && version.Version != "" {
		existing.Object["appVersion"] = version.Version
	}

	updated, err := dynamicClient.Resource(gvr).Namespace(packageRef.Context.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("update", gvr.Resource, packageRef.Identifier, err)
	}

	return &corev1.InstalledPackageReference{
		Context: &corev1.Context{
			Namespace: updated.GetNamespace(),
			Cluster:   s.kubeappsCluster,
		},
		Identifier: updated.GetName(),
		Plugin:     GetPluginDetail(),
	}, nil
}

// deleteRelease deletes a custom resource instance
func (s *Server) deleteRelease(ctx context.Context, headers http.Header, packageRef *corev1.InstalledPackageReference) error {
	// Split identifier to get kind and name
	parts := strings.Split(packageRef.Identifier, "~")
	if len(parts) != 2 {
		return fmt.Errorf("invalid identifier format: %s", packageRef.Identifier)
	}
	kind := parts[0]
	name := parts[1]

	// Find resource config
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Kind == kind {
			resourceConfig = &res
			break
		}
	}
	if resourceConfig == nil {
		return fmt.Errorf("resource config not found for kind: %s", kind)
	}

	// Get GVR
	gvr := schema.GroupVersionResource{
		Group:    "apps.cozystack.io",
		Version:  "v1alpha1",
		Resource: resourceConfig.Application.Plural,
	}

	// Get dynamic client
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return fmt.Errorf("failed to get dynamic client: %w", err)
	}

	// Delete the resource
	err = dynamicClient.Resource(gvr).Namespace(packageRef.Context.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return connecterror.FromK8sError("delete", gvr.Resource, name, err)
	}

	return nil
}

func (s *Server) installedPackageDetail(ctx context.Context, headers http.Header, key types.NamespacedName) (*corev1.InstalledPackageDetail, error) {
	// Split the identifier to get kind and name
	parts := strings.Split(key.Name, "~")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid identifier format: %s", key.Name)
	}
	kind := parts[0]
	name := parts[1]

	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	// Find the resource config for the kind
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Kind == kind {
			resourceConfig = &res
			break
		}
	}
	if resourceConfig == nil {
		return nil, fmt.Errorf("resource config not found for kind: %s", kind)
	}

	// Get the GVR for the resource type
	gvr := schema.GroupVersionResource{
		Group:    "apps.cozystack.io",
		Version:  "v1alpha1",
		Resource: resourceConfig.Application.Plural,
	}

	// Get the resource
	obj, err := dynamicClient.Resource(gvr).Namespace(key.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("get", gvr.Resource, name, err)
	}

	version, _, _ := unstructured.NestedString(obj.Object, "appVersion")
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	valuesJson, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec: %w", err)
	}

	availablePkgRef := &corev1.AvailablePackageReference{
		Context: &corev1.Context{
			Namespace: resourceConfig.Release.Chart.SourceRef.Namespace,
			Cluster:   s.kubeappsCluster,
		},
		Identifier: fmt.Sprintf("%s/%s",
			resourceConfig.Release.Chart.SourceRef.Name,
			resourceConfig.Release.Chart.Name),
		Plugin: GetPluginDetail(),
	}

	return &corev1.InstalledPackageDetail{
		InstalledPackageRef: &corev1.InstalledPackageReference{
			Context: &corev1.Context{
				Namespace: key.Namespace,
				Cluster:   s.kubeappsCluster,
			},
			Identifier: fmt.Sprintf("%s~%s", kind, name),
			Plugin:     GetPluginDetail(),
		},
		Name: name,
		CurrentVersion: &corev1.PackageAppVersion{
			PkgVersion: version,
			AppVersion: version,
		},
		AvailablePackageRef: availablePkgRef,
		ValuesApplied:       string(valuesJson),
		Status:              getResourceStatus(obj),
	}, nil
}
