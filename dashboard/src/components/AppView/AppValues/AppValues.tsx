// Copyright 2019-2022 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0

import MonacoEditor from "react-monaco-editor";
import { useSelector } from "react-redux";
import { SupportedThemes } from "shared/Config";
import { useMemo } from "react";
import { parseToYamlNode, toStringYamlNode } from "shared/yamlUtils";
import { IStoreState } from "shared/types";
import "./AppValues.css";

interface IAppValuesProps {
  values: string;
}

function AppValues(props: IAppValuesProps) {
  const {
    config: { theme },
  } = useSelector((state: IStoreState) => state);

  const displayValues = useMemo(() => {
    if (!props.values) return props.values;
    try {
      const node = parseToYamlNode(props.values);
      return toStringYamlNode(node);
    } catch {
      return props.values;
    }
  }, [props.values]);

  let values = <p>The current application was installed without specifying any values</p>;
  if (displayValues) {
    values = (
      <MonacoEditor
        language="yaml"
        theme={theme === SupportedThemes.dark ? "vs-dark" : "light"}
        className="installation-values"
        height="50vh"
        value={displayValues}
        options={{ automaticLayout: true, readOnly: true }}
      />
    );
  }

  return (
    <section aria-labelledby="installation-values">
      <h3 className="section-title" id="installation-values">
        Installation Values
      </h3>
      {values}
    </section>
  );
}

export default AppValues;
