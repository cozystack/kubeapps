// Copyright 2021-2024 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bufbuild/connect-go"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	corev1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/fluxv2/packages/v1alpha1/cache"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/fluxv2/packages/v1alpha1/common"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/connecterror"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/plugins/pkg/pkgutils"
	"github.com/vmware-tanzu/kubeapps/pkg/chart/models"
	httpclient "github.com/vmware-tanzu/kubeapps/pkg/http-client"
	"github.com/vmware-tanzu/kubeapps/pkg/tarutil"
	"helm.sh/helm/v3/pkg/chart"
	"k8s.io/apimachinery/pkg/types"
	log "k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

func (s *Server) getChartInCluster(ctx context.Context, headers http.Header, key types.NamespacedName) (*sourcev1.HelmChart, error) {
	client, err := s.getClient(headers, key.Namespace)
	if err != nil {
		return nil, err
	}
	var chartObj sourcev1.HelmChart
	if err = client.Get(ctx, key, &chartObj); err != nil {
		return nil, connecterror.FromK8sError("get", "HelmChart", key.String(), err)
	}
	return &chartObj, nil
}

// Helper functions for Kind-based package management
func findKindByChartName(config *common.FluxPluginConfig, chartName string) (string, error) {
	for _, res := range config.Resources {
		if res.Release.Chart.Name == chartName {
			return res.Application.Kind, nil
		}
	}
	return "", fmt.Errorf("no Kind found for chart: %s", chartName)
}

func findChartNameByKind(config *common.FluxPluginConfig, kind string) (string, error) {
	for _, res := range config.Resources {
		if res.Application.Kind == kind {
			return res.Release.Chart.Name, nil
		}
	}
	return "", fmt.Errorf("no chart found for Kind: %s", kind)
}

// TODO (gfichtenholt) this func is too long. Break it up
func (s *Server) availableChartDetail(ctx context.Context, headers http.Header, packageRef *corev1.AvailablePackageReference, chartVersion string) (*corev1.AvailablePackageDetail, error) {
	log.Infof("+availableChartDetail(%s, %s)", packageRef, chartVersion)

	repoName, kind, err := pkgutils.SplitPackageIdentifier(packageRef.Identifier)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	log.Infof("Split identifier - repo: [%s], kind: [%s]", repoName, kind)

	// Find resource config by case-insensitive comparison
	var resourceConfig *common.ConfigResource
	for _, res := range s.pluginConfig.Resources {
		if res.Application.Kind == kind {
			resourceConfig = &res
			break
		}
	}
	if resourceConfig == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("Resource config not found for Kind: %s", kind))
	}
	log.Infof("Found resource config for kind [%s], chart name: [%s]", kind, resourceConfig.Release.Chart.Name)

	// Get chart name from config
	chartName := resourceConfig.Release.Chart.Name

	// Use repository from config
	repo := types.NamespacedName{
		Namespace: resourceConfig.Release.Chart.SourceRef.Namespace,
		Name:      resourceConfig.Release.Chart.SourceRef.Name,
	}
	log.Infof("Using repo from config: [%s/%s]", repo.Namespace, repo.Name)

	// Get the repository
	repoObj, err := s.getRepoInCluster(ctx, headers, repo)
	if err != nil {
		return nil, err
	} else if !isRepoReady(*repoObj) {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("Repository [%s] is not in Ready state", repo))
	}

	// Use chartName instead of Kind for cache key since chart files are stored by chart name
	chartID := fmt.Sprintf("%s/%s", repo.Name, chartName)
	log.Infof("Using chart ID for cache: [%s]", chartID)

	// Try cache first
	var byteArray []byte
	if chartVersion != "" {
		if key, err := s.chartCache.KeyFor(repo.Namespace, chartID, chartVersion); err != nil {
			return nil, err
		} else {
			log.Infof("Looking up cache key: [%s]", key)
			if byteArray, err = s.chartCache.Fetch(key); err != nil {
				return nil, err
			}
		}
	}

	if byteArray == nil {
		log.Info("Cache miss or no version specified, fetching chart model")
		// Get chart model
		chartModel, err := s.getChartModel(ctx, headers, repo, chartName)
		if err != nil {
			return nil, err
		} else if chartModel == nil {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("Chart [%s] not found", chartName))
		}

		if chartVersion == "" {
			chartVersion = chartModel.ChartVersions[0].Version
			log.Infof("Using default version: [%s]", chartVersion)
		}

		var key string
		if key, err = s.chartCache.KeyFor(repo.Namespace, chartID, chartVersion); err != nil {
			return nil, err
		}
		log.Infof("Cache key for download: [%s]", key)

		var fn cache.DownloadChartFn
		if chartModel.Repo.Type == "oci" {
			if ociRepo, err := s.newOCIChartRepositoryAndLogin(ctx, *repoObj); err != nil {
				return nil, err
			} else {
				fn = downloadOCIChartFn(ociRepo)
			}
		} else {
			if opts, err := s.httpClientOptionsForRepo(ctx, headers, repo); err != nil {
				return nil, err
			} else {
				fn = downloadHttpChartFn(opts)
			}
		}
		if byteArray, err = s.chartCache.Get(key, chartModel, fn); err != nil {
			return nil, err
		}

		if byteArray == nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("Failed to load details for chart [%s], version [%s]", chartModel.ID, chartVersion))
		}
	}

	chartDetail, err := tarutil.FetchChartDetailFromTarball(bytes.NewReader(byteArray))
	if err != nil {
		return nil, err
	}

	pkgDetail, err := availablePackageDetailFromChartDetail(chartID, chartDetail)
	if err != nil {
		return nil, err
	}

	// Fix up package references to use Kind
	pkgDetail.Name = kind
	pkgDetail.DisplayName = kind
	pkgDetail.AvailablePackageRef = &corev1.AvailablePackageReference{
		Context: &corev1.Context{
			Namespace: packageRef.Context.Namespace,
			Cluster:   s.kubeappsCluster,
		},
		Plugin:     GetPluginDetail(),
		Identifier: fmt.Sprintf("%s/%s", repo.Name, kind),
	}

	repoUrl := repoObj.Spec.URL
	if repoUrl == "" {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("Missing required field spec.url on repository %q", repo))
	}
	pkgDetail.RepoUrl = repoUrl

	return pkgDetail, nil
}

func (s *Server) getChartModel(ctx context.Context, headers http.Header, repoName types.NamespacedName, chartName string) (*models.Chart, error) {
	if s.repoCache == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("Server cache has not been properly initialized"))
	} else if ok, err := s.hasAccessToNamespace(ctx, headers, common.GetChartsGvr(), repoName.Namespace); err != nil {
		return nil, err
	} else if !ok {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("User has no [get] access for HelmCharts in namespace [%s]", repoName.Namespace))
	}

	key := s.repoCache.KeyForNamespacedName(repoName)
	value, err := s.repoCache.Get(key)
	if err != nil {
		return nil, err
	} else {
		typedValue, err := s.repoCacheEntryFromUntyped(key, value)
		if err != nil {
			return nil, err
		} else if typedValue == nil {
			return nil, nil
		} else {
			for _, chart := range typedValue.Charts {
				if chart.Name == chartName {
					return &chart, nil // found it
				}
			}
		}
	}
	return nil, nil
}

func passesFilter(chart models.Chart, filters *corev1.FilterOptions) bool {
	if filters == nil {
		return true
	}
	ok := true
	if categories := filters.GetCategories(); len(categories) > 0 {
		ok = false
		for _, cat := range categories {
			if cat == chart.Category {
				ok = true
				break
			}
		}
	}
	if ok {
		if appVersion := filters.GetAppVersion(); len(appVersion) > 0 {
			ok = appVersion == chart.ChartVersions[0].AppVersion
		}
	}
	if ok {
		if pkgVersion := filters.GetPkgVersion(); len(pkgVersion) > 0 {
			ok = pkgVersion == chart.ChartVersions[0].Version
		}
	}
	if ok {
		if query := filters.GetQuery(); len(query) > 0 {
			if strings.Contains(chart.Name, query) {
				return true
			}
			if strings.Contains(chart.Description, query) {
				return true
			}
			for _, keyword := range chart.Keywords {
				if strings.Contains(keyword, query) {
					return true
				}
			}
			for _, source := range chart.Sources {
				if strings.Contains(source, query) {
					return true
				}
			}
			for _, maintainer := range chart.Maintainers {
				if strings.Contains(maintainer.Name, query) {
					return true
				}
			}
			// could not find a match for the query text
			ok = false
		}
	}
	return ok
}

func filterAndPaginateCharts(filters *corev1.FilterOptions, pageSize int32, itemOffset int, charts map[string][]models.Chart, pluginConfig *common.FluxPluginConfig) ([]*corev1.AvailablePackageSummary, error) {
	summaries := make([]*corev1.AvailablePackageSummary, 0)
	i := 0
	startAt := -1
	if pageSize > 0 {
		startAt = itemOffset
	}
	for _, packages := range charts {
		for p := range packages {
			chart := packages[p]
			if passesFilter(chart, filters) {
				i++
				if startAt < i {
					// Find resource config for this chart
					resourceConfig := pluginConfig.FindResourceByChart(chart.Repo.Name, chart.Name)
					if resourceConfig == nil {
						continue
					}

					pkg, err := pkgutils.AvailablePackageSummaryFromChart(&chart, GetPluginDetail())
					if err != nil {
						return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("Unable to parse chart to an AvailablePackageSummary: %w", err))
					}

					// Update the identifier to use Kind
					pkg.AvailablePackageRef.Identifier = fmt.Sprintf("%s/%s", resourceConfig.Application.Kind, chart.Name)
					pkg.Name = resourceConfig.Application.Kind
					pkg.DisplayName = resourceConfig.Application.Kind

					summaries = append(summaries, pkg)
					if pageSize > 0 && len(summaries) == int(pageSize) {
						return summaries, nil
					}
				}
			}
		}
	}
	return summaries, nil
}

func availablePackageDetailFromChartDetail(chartID string, chartDetail map[string]string) (*corev1.AvailablePackageDetail, error) {
	chartYaml, ok := chartDetail[models.ChartYamlKey]
	// TODO (gfichtenholt): if there is no chart yaml (is that even possible?),
	// fall back to chart info from repo index.yaml
	if !ok || chartYaml == "" {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("No chart manifest found for chart: [%s]", chartID))
	}
	var chartMetadata chart.Metadata
	err := yaml.Unmarshal([]byte(chartYaml), &chartMetadata)
	if err != nil {
		return nil, err
	}

	maintainers := []*corev1.Maintainer{}
	for _, maintainer := range chartMetadata.Maintainers {
		m := &corev1.Maintainer{Name: maintainer.Name, Email: maintainer.Email}
		maintainers = append(maintainers, m)
	}

	var categories []string
	category, found := chartMetadata.Annotations["category"]
	if found && category != "" {
		categories = []string{category}
	}

	pkg := &corev1.AvailablePackageDetail{
		Name: chartMetadata.Name,
		Version: &corev1.PackageAppVersion{
			PkgVersion: chartMetadata.Version,
			AppVersion: chartMetadata.AppVersion,
		},
		HomeUrl:          chartMetadata.Home,
		IconUrl:          chartMetadata.Icon,
		DisplayName:      chartMetadata.Name,
		ShortDescription: chartMetadata.Description,
		Categories:       categories,
		Readme:           chartDetail[models.ReadmeKey],
		DefaultValues:    chartDetail[models.DefaultValuesKey],
		ValuesSchema:     chartDetail[models.SchemaKey],
		SourceUrls:       chartMetadata.Sources,
		Maintainers:      maintainers,
		AvailablePackageRef: &corev1.AvailablePackageReference{
			Identifier: chartID,
			Plugin:     GetPluginDetail(),
			Context:    &corev1.Context{},
		},
	}
	// TODO: (gfichtenholt) LongDescription?

	// note, the caller will set pkg.AvailablePackageRef namespace as that information
	// is not included in the tarball
	return pkg, nil
}

func downloadHttpChartFn(options *common.HttpClientOptions) func(chartID, chartUrl, chartVersion string) ([]byte, error) {
	return func(chartID, chartUrl, chartVersion string) ([]byte, error) {
		client, headers, err := common.NewHttpClientAndHeaders(options)
		if err != nil {
			return nil, err
		}

		reader, _, err := httpclient.GetStream(chartUrl, client, headers)
		if reader != nil {
			defer reader.Close()
		}
		if err != nil {
			return nil, err
		}

		chartTgz, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}

		return chartTgz, nil
	}
}
