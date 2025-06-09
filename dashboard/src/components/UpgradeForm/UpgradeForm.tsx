// Copyright 2018-2023 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0

import actions from "actions";
import AlertGroup from "components/AlertGroup";
import AvailablePackageDetailExcerpt from "components/Catalog/AvailablePackageDetailExcerpt";
import Column from "components/Column";
import DeploymentFormBody from "components/DeploymentForm/DeploymentFormBody";
import LoadingWrapper from "components/LoadingWrapper";
import PackageHeader from "components/PackageHeader/PackageHeader";
import Row from "components/Row";
import * as jsonpatch from "fast-json-patch";
import { usePush } from "hooks/push";
import { useEffect, useRef, useState } from "react";
import { useDispatch, useSelector } from "react-redux";
import { Action } from "redux";
import { ThunkDispatch } from "redux-thunk";
import { IStoreState } from "shared/types";
import * as url from "shared/url";
import { deleteValue, parseToJS, setValue } from "shared/yamlUtils";

export interface IUpgradeFormProps {
  version?: string;
}

// TODO(agamez): Use the YAML-node based functions to avoid re-parse the yaml again and again
function applyModifications(mods: jsonpatch.Operation[], values: string) {
  // And we add any possible change made to the original version
  if (mods.length) {
    mods.forEach(modification => {
      if (modification.op === "remove") {
        values = deleteValue(values, modification.path);
      } else {
        // Transform the modification as a ReplaceOperation to read its value
        const value = (modification as jsonpatch.ReplaceOperation<any>).value;
        values = setValue(values, modification.path, value);
      }
    });
  }
  return values;
}

function UpgradeForm(_: IUpgradeFormProps) {
  const dispatch: ThunkDispatch<IStoreState, null, Action> = useDispatch();

  const {
    apps: {
      selected: installedAppInstalledPackageDetail,
      isFetching: appsIsFetching,
      error,
      selectedDetails: installedAppAvailablePackageDetail,
    },
    packages: { isFetching: chartsIsFetching, selected: selectedPackage },
  } = useSelector((state: IStoreState) => state);

  const isFetching = appsIsFetching || chartsIsFetching;
  const { availablePackageDetail, versions, schema, values, pkgVersion } = selectedPackage;

  const [appValues, setAppValues] = useState("");
  const [modifications, setModifications] = useState(
    undefined as undefined | jsonpatch.Operation[],
  );
  const [deployedValues, setDeployedValues] = useState("");
  const [isDeploying, setIsDeploying] = useState(false);
  const [valuesModified, setValuesModified] = useState(false);
  const formRef = useRef<HTMLFormElement>(null);

  /* Load the list of versions once */
  useEffect(() => {
    if (selectedPackage.versions.length === 0) {
      dispatch(
        actions.availablepackages.fetchAvailablePackageVersions(
          installedAppInstalledPackageDetail?.availablePackageRef,
        ),
      );
    }
  }, [
    dispatch,
    installedAppInstalledPackageDetail?.availablePackageRef,
    selectedPackage.versions.length,
  ]);

  /* After versions are loaded, always select the latest (assumes list is already sorted DESC). */
  useEffect(() => {
    if (
      versions.length > 0 &&
      availablePackageDetail?.version?.pkgVersion !== versions[0].pkgVersion
    ) {
      dispatch(
        actions.availablepackages.fetchAndSelectAvailablePackageDetail(
          installedAppInstalledPackageDetail?.availablePackageRef,
          versions[0].pkgVersion,
        ),
      );
    }
  }, [
    dispatch,
    versions,
    availablePackageDetail?.version?.pkgVersion,
    installedAppInstalledPackageDetail?.availablePackageRef,
  ]);

  useEffect(() => {
    if (
      installedAppAvailablePackageDetail?.defaultValues &&
      installedAppInstalledPackageDetail?.valuesApplied &&
      installedAppInstalledPackageDetail?.valuesApplied !== "null" &&
      !modifications
    ) {
      // Calculate modifications from the default values
      const defaultValuesObj = parseToJS(installedAppAvailablePackageDetail?.defaultValues);
      const deployedValuesObj = parseToJS(installedAppInstalledPackageDetail?.valuesApplied);
      const newModifications = jsonpatch.compare(defaultValuesObj as any, deployedValuesObj as any);
      const values = applyModifications(
        newModifications,
        installedAppAvailablePackageDetail?.defaultValues,
      );
      setModifications(newModifications);
      setAppValues(values);
    }
  }, [
    installedAppAvailablePackageDetail?.defaultValues,
    installedAppInstalledPackageDetail?.valuesApplied,
    modifications,
  ]);

  useEffect(() => {
    if (installedAppAvailablePackageDetail?.defaultValues) {
      // Apply modifications to deployed values
      const values = applyModifications(
        modifications || [],
        installedAppAvailablePackageDetail?.defaultValues,
      );
      setDeployedValues(values);
    }
  }, [installedAppAvailablePackageDetail?.defaultValues, modifications]);

  useEffect(() => {
    if (!valuesModified && values) {
      // Apply modifications to the new selected version
      const newAppValues = modifications?.length
        ? applyModifications(modifications, values)
        : values;
      setAppValues(newAppValues);
    }
  }, [values, modifications, valuesModified]);

  const setValuesModifiedTrue = () => {
    setValuesModified(true);
  };

  const handleValuesChange = (value: string) => {
    setAppValues(value);
  };

  const push = usePush();

  const handleDeploy = async (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    setIsDeploying(true);
    if (
      availablePackageDetail &&
      installedAppInstalledPackageDetail?.installedPackageRef &&
      installedAppInstalledPackageDetail?.availablePackageRef?.context?.namespace
    ) {
      const deployedSuccess = await dispatch(
        actions.installedpackages.updateInstalledPackage(
          installedAppInstalledPackageDetail?.installedPackageRef,
          availablePackageDetail,
          appValues,
          schema,
        ),
      );
      setIsDeploying(false);
      if (deployedSuccess) {
        push(url.app.apps.get(installedAppInstalledPackageDetail?.installedPackageRef));
      }
    }
  };

  /* eslint-disable jsx-a11y/label-has-associated-control */
  return (
    <section>
      {isDeploying && (
        <h3 className="center" style={{ marginBottom: "1.2rem" }}>
          The application is being upgraded, please wait...
        </h3>
      )}
      {!isFetching && error && (
        <AlertGroup status="danger">An error occurred: {error?.message}.</AlertGroup>
      )}
      <LoadingWrapper
        loaded={!isDeploying && !isFetching && versions?.length > 0 && !!availablePackageDetail}
      >
        {(!isFetching && versions?.length === 0) || !availablePackageDetail ? (
          <></>
        ) : (
          <>
            <PackageHeader
              releaseName={installedAppInstalledPackageDetail?.installedPackageRef?.identifier}
              availablePackageDetail={availablePackageDetail}
              versions={versions}
              onSelect={() => undefined}
              currentVersion={installedAppAvailablePackageDetail?.version?.pkgVersion}
              selectedVersion={pkgVersion}
              hideVersionsSelector={true}
            />
            <LoadingWrapper
              loaded={
                !isDeploying && !isFetching && versions?.length > 0 && !!availablePackageDetail
              }
            >
              {!installedAppInstalledPackageDetail?.availablePackageRef?.identifier ||
              !installedAppInstalledPackageDetail?.currentVersion?.pkgVersion ? (
                <></>
              ) : (
                <>
                  <Row>
                    <Column span={3}>
                      <AvailablePackageDetailExcerpt pkg={availablePackageDetail} />
                    </Column>
                    <Column span={9}>
                      <form onSubmit={handleDeploy} ref={formRef}>
                        <DeploymentFormBody
                          deploymentEvent="upgrade"
                          packageId={
                            installedAppInstalledPackageDetail?.availablePackageRef?.identifier
                          }
                          packageVersion={
                            installedAppInstalledPackageDetail?.currentVersion?.pkgVersion
                          }
                          deployedValues={deployedValues}
                          packagesIsFetching={isFetching}
                          selected={selectedPackage}
                          setValues={handleValuesChange}
                          appValues={appValues}
                          setValuesModified={setValuesModifiedTrue}
                          formRef={formRef}
                        />
                      </form>
                    </Column>
                  </Row>
                </>
              )}
            </LoadingWrapper>
          </>
        )}
      </LoadingWrapper>
    </section>
  );
}

export default UpgradeForm;
