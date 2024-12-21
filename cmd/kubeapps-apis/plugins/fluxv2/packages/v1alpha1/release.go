package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/connecterror"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	log "k8s.io/klog/v2"
)

// getAvailableResourceTypes returns a list of available custom resource types
func (s *Server) getAvailableResourceTypes(ctx context.Context, headers http.Header) ([]schema.GroupVersionResource, error) {
	// Get discovery client with user auth
	discoveryClient, err := s.clientGetter.Typed(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get discovery client: %w", err)
	}

	resources, err := discoveryClient.Discovery().ServerResourcesForGroupVersion("apps.cozystack.io/v1alpha1")
	if err != nil {
		return nil, fmt.Errorf("failed to discover resources: %w", err)
	}

	var gvrs []schema.GroupVersionResource
	for _, r := range resources.APIResources {
		gvrs = append(gvrs, schema.GroupVersionResource{
			Group:    "apps.cozystack.io",
			Version:  "v1alpha1",
			Resource: r.Name,
		})
	}
	return gvrs, nil
}

// listReleasesInCluster returns all installed applications
func (s *Server) listReleasesInCluster(ctx context.Context, headers http.Header, namespace string) ([]*unstructured.Unstructured, error) {
	var allReleases []*unstructured.Unstructured

	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvrs, err := s.getAvailableResourceTypes(ctx, headers)
	if err != nil {
		return nil, err
	}

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
				summary, err := s.installedPkgSummaryFromRelease(ctx, r)
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

func (s *Server) installedPkgSummaryFromRelease(ctx context.Context, rel *unstructured.Unstructured) (*corev1.InstalledPackageSummary, error) {
	name := rel.GetName()
	namespace := rel.GetNamespace()
	version, _, _ := unstructured.NestedString(rel.Object, "appVersion")

	return &corev1.InstalledPackageSummary{
		InstalledPackageRef: &corev1.InstalledPackageReference{
			Context: &corev1.Context{
				Namespace: namespace,
				Cluster:   s.kubeappsCluster,
			},
			Identifier: name,
			Plugin:     GetPluginDetail(),
		},
		Name: name,
		PkgVersionReference: &corev1.VersionReference{
			Version: version,
		},
		Status: getResourceStatus(rel),
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

	gvr, err := s.getGVRFromPackageID(ctx, headers, packageRef.Identifier)
	if err != nil {
		return nil, err
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("%s/%s", gvr.Group, gvr.Version),
			"kind":       strings.Title(gvr.Resource[:len(gvr.Resource)-1]),
			"metadata": map[string]interface{}{
				"name":      targetName.Name,
				"namespace": targetName.Namespace,
			},
		},
	}

	if values != "" {
		var specValues map[string]interface{}
		if err := json.Unmarshal([]byte(values), &specValues); err != nil {
			return nil, fmt.Errorf("failed to parse values: %w", err)
		}
		obj.Object["spec"] = specValues
	}

	if version != nil && version.Version != "" {
		obj.Object["appVersion"] = version.Version
	}

	created, err := dynamicClient.Resource(gvr).Namespace(targetName.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, connecterror.FromK8sError("create", gvr.Resource, targetName.String(), err)
	}

	return &corev1.InstalledPackageReference{
		Context: &corev1.Context{
			Namespace: created.GetNamespace(),
			Cluster:   s.kubeappsCluster,
		},
		Identifier: created.GetName(),
		Plugin:     GetPluginDetail(),
	}, nil
}

func (s *Server) getGVRFromPackageID(ctx context.Context, headers http.Header, packageID string) (schema.GroupVersionResource, error) {
	parts := strings.Split(packageID, "/")
	if len(parts) != 2 {
		return schema.GroupVersionResource{}, fmt.Errorf("invalid package ID format: %s", packageID)
	}
	repoName := parts[0]
	chartName := parts[1]

	// Find matching resource from config
	resource := s.pluginConfig.FindResourceByChart(repoName, chartName)
	if resource == nil {
		return schema.GroupVersionResource{}, fmt.Errorf("no matching resource type found for package %s/%s", repoName, chartName)
	}

	return schema.GroupVersionResource{
		Group:    "apps.cozystack.io",
		Version:  "v1alpha1",
		Resource: resource.Application.Plural,
	}, nil
}

// updateRelease updates an existing custom resource instance
func (s *Server) updateRelease(ctx context.Context, headers http.Header, packageRef *corev1.InstalledPackageReference, version *corev1.VersionReference, values string) (*corev1.InstalledPackageReference, error) {
	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr, err := s.getGVRFromPackageID(ctx, headers, packageRef.Identifier)
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
	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvr, err := s.getGVRFromPackageID(ctx, headers, packageRef.Identifier)
	if err != nil {
		return err
	}

	err = dynamicClient.Resource(gvr).Namespace(packageRef.Context.Namespace).Delete(ctx, packageRef.Identifier, metav1.DeleteOptions{})
	if err != nil {
		return connecterror.FromK8sError("delete", gvr.Resource, packageRef.Identifier, err)
	}

	return nil
}

func (s *Server) installedPackageDetail(ctx context.Context, headers http.Header, key types.NamespacedName) (*corev1.InstalledPackageDetail, error) {
	// Get dynamic client with user auth
	dynamicClient, err := s.clientGetter.Dynamic(headers, s.kubeappsCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to get dynamic client: %w", err)
	}

	gvrs, err := s.getAvailableResourceTypes(ctx, headers)
	if err != nil {
		return nil, err
	}

	for _, gvr := range gvrs {
		obj, err := dynamicClient.Resource(gvr).Namespace(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
		if err == nil {
			version, _, _ := unstructured.NestedString(obj.Object, "appVersion")
			spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
			valuesJson, err := json.Marshal(spec)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal spec: %w", err)
			}

			return &corev1.InstalledPackageDetail{
				InstalledPackageRef: &corev1.InstalledPackageReference{
					Context: &corev1.Context{
						Namespace: key.Namespace,
						Cluster:   s.kubeappsCluster,
					},
					Identifier: key.Name,
					Plugin:     GetPluginDetail(),
				},
				Name: key.Name,
				CurrentVersion: &corev1.PackageAppVersion{
					PkgVersion: version,
					AppVersion: version,
				},
				ValuesApplied: string(valuesJson),
				Status:        getResourceStatus(obj),
			}, nil
		}
	}

	return nil, fmt.Errorf("package not found: %s", key.String())
}
