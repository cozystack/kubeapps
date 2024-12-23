package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bufbuild/connect-go"
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	corev1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/fluxv2/packages/v1alpha1/common"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/connecterror"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/pkgutils"
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

	// Find resource config by kind
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

	// Create identifier using Kind
	identifier := fmt.Sprintf("%s/%s", kind, name)

	availablePkgRef := &corev1.AvailablePackageReference{
		Context: &corev1.Context{
			Namespace: resourceConfig.Release.Chart.SourceRef.Namespace,
			Cluster:   s.kubeappsCluster,
		},
		Identifier: fmt.Sprintf("%s/%s",
			resourceConfig.Release.Chart.SourceRef.Name,
			kind),
		Plugin: GetPluginDetail(),
	}

	pkgDetail, err := s.getAvailablePackageDetail(ctx, headers, availablePkgRef)
	if err != nil {
		return nil, err
	}

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
	repoName, kind, err := pkgutils.SplitPackageIdentifier(ref.Identifier)
	if err != nil {
		return nil, err
	}

	chartName, err := findChartNameByKind(s.pluginConfig, kind)
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
		Name:        kind,
		DisplayName: kind,
		//DisplayName:      chart.Name,
		IconUrl:          chart.Icon,
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
func (s *Server) newRelease(ctx context.Context, headers http.Header, packageRef *corev1.AvailablePackageReference, targetName types.NamespacedName, versionRef *corev1.VersionReference, values string) (*corev1.InstalledPackageReference, error) {
	log.Infof("+newRelease (cluster: [%s], namespace: [%s], targetName: [%s], version: [%v])",
		s.kubeappsCluster, targetName.Namespace, targetName.Name, versionRef)

	_, kind, err := pkgutils.SplitPackageIdentifier(packageRef.Identifier)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Find the resource config for this Kind
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Kind == kind {
			resourceConfig = &res
			break
		}
	}
	if resourceConfig == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("No resource configuration found for Kind: %s", kind))
	}

	// Create the resource
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "apps.cozystack.io/v1alpha1",
			"kind":       kind,
			"metadata": map[string]interface{}{
				"name":      targetName.Name,
				"namespace": targetName.Namespace,
			},
			"spec": make(map[string]interface{}),
		},
	}

	// Set version if specified
	if versionRef != nil && versionRef.Version != "" {
		if err := unstructured.SetNestedField(obj.Object, versionRef.Version, "appVersion"); err != nil {
			return nil, fmt.Errorf("failed to set version: %w", err)
		}
	}

	// Set values if provided
	if values != "" {
		var specValues map[string]interface{}
		if err := json.Unmarshal([]byte(values), &specValues); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("Invalid values: %w", err))
		}
		obj.Object["spec"] = specValues
	}

	// Get dynamic client
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	// Get GVR for the resource
	gvr := schema.GroupVersionResource{
		Group:    "apps.cozystack.io",
		Version:  "v1alpha1",
		Resource: resourceConfig.Application.Plural,
	}

	// Create the resource
	created, err := dynamicClient.Resource(gvr).Namespace(targetName.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("create", kind, targetName.String(), err)
	}

	return &corev1.InstalledPackageReference{
		Context: &corev1.Context{
			Cluster:   s.kubeappsCluster,
			Namespace: created.GetNamespace(),
		},
		Identifier: fmt.Sprintf("%s/%s", kind, created.GetName()),
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

func (s *Server) getGVRFromPackageID(packageID string) (schema.GroupVersionResource, *common.ConfigResource, error) {
	parts := strings.Split(packageID, "/")
	if len(parts) != 2 {
		return schema.GroupVersionResource{}, nil, fmt.Errorf("invalid package ID format: %s", packageID)
	}
	repoName := parts[0]
	chartName := parts[1]

	// Find matching resource from config based on chart name
	for _, res := range s.pluginConfig.Resources {
		if res.Release.Chart.SourceRef.Name == repoName &&
			res.Release.Chart.Name == chartName {
			return schema.GroupVersionResource{
				Group:    "apps.cozystack.io",
				Version:  "v1alpha1",
				Resource: res.Application.Plural,
			}, &res, nil
		}
	}

	return schema.GroupVersionResource{}, nil, fmt.Errorf("no matching resource type found for package %s/%s", repoName, chartName)
}

// updateRelease updates an existing custom resource instance
func (s *Server) updateRelease(ctx context.Context, headers http.Header, installedRef *corev1.InstalledPackageReference, versionRef *corev1.VersionReference, values string) (*corev1.InstalledPackageReference, error) {
	log.Infof("+updateRelease (cluster: [%s], namespace: [%s], installedRef: [%s])",
		s.kubeappsCluster, installedRef.Context.Namespace, installedRef.Identifier)

	// Split identifier to get Kind and name
	parts := strings.Split(installedRef.Identifier, "/")
	if len(parts) != 2 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("Invalid identifier format. Expected <Kind>/<name>, got: %s", installedRef.Identifier))
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
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("No resource configuration found for Kind: %s", kind))
	}

	// Get dynamic client
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	// Get GVR for the resource
	gvr := schema.GroupVersionResource{
		Group:    "apps.cozystack.io",
		Version:  "v1alpha1",
		Resource: resourceConfig.Application.Plural,
	}

	// Get the existing resource
	obj, err := dynamicClient.Resource(gvr).Namespace(installedRef.Context.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("get", kind, name, err)
	}

	// Update version if specified
	if versionRef != nil && versionRef.Version != "" {
		if err := unstructured.SetNestedField(obj.Object, versionRef.Version, "appVersion"); err != nil {
			return nil, fmt.Errorf("failed to set version: %w", err)
		}
	}

	// Update values if specified
	if values != "" {
		var specValues map[string]interface{}
		if err := json.Unmarshal([]byte(values), &specValues); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("Invalid values: %w", err))
		}
		obj.Object["spec"] = specValues
	}

	// Update the resource
	_, err = dynamicClient.Resource(gvr).Namespace(installedRef.Context.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("update", kind, name, err)
	}

	return installedRef, nil
}

// deleteRelease deletes a custom resource instance
func (s *Server) deleteRelease(ctx context.Context, headers http.Header, installedRef *corev1.InstalledPackageReference) error {
	log.Infof("+deleteRelease (cluster: [%s], namespace: [%s], installedRef: [%s])",
		s.kubeappsCluster, installedRef.Context.Namespace, installedRef.Identifier)

	// Split identifier to get Kind and name
	parts := strings.Split(installedRef.Identifier, "/")
	if len(parts) != 2 {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("Invalid identifier format. Expected <Kind>/<name>, got: %s", installedRef.Identifier))
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
		return connect.NewError(connect.CodeNotFound,
			fmt.Errorf("No resource configuration found for Kind: %s", kind))
	}

	key := types.NamespacedName{
		Namespace: installedRef.Context.Namespace,
		Name:      name,
	}
	client, err := s.getClient(headers, key.Namespace)
	if err != nil {
		return err
	}

	helmRelease := &helmv2.HelmRelease{
		TypeMeta: metav1.TypeMeta{
			Kind:       kind,
			APIVersion: "apps.cozystack.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: key.Namespace,
		},
	}

	if err = client.Delete(ctx, helmRelease); err != nil {
		return connecterror.FromK8sError("delete", kind, key.String(), err)
	}

	return nil
}

func (s *Server) installedPackageDetail(ctx context.Context, headers http.Header, key types.NamespacedName) (*corev1.InstalledPackageDetail, error) {
	// Split the key name to get chart name and resource name
	parts := strings.Split(key.Name, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid identifier format: %s, expected ChartName/Name", key.Name)
	}
	kind := parts[0]
	name := parts[1]

	// Find the resource config for this Kind
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Kind == kind {
			resourceConfig = &res
			break
		}
	}
	if resourceConfig == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("No resource configuration found for Kind: %s", kind))
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
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
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
			resourceConfig.Application.Kind),
		Plugin: GetPluginDetail(),
	}

	return &corev1.InstalledPackageDetail{
		InstalledPackageRef: &corev1.InstalledPackageReference{
			Context: &corev1.Context{
				Namespace: key.Namespace,
				Cluster:   s.kubeappsCluster,
			},
			Identifier: fmt.Sprintf("%s/%s", kind, name),
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
