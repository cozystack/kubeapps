import { CdsButton } from "@cds/react/button";
import { CdsControlMessage } from "@cds/react/forms";
import { CdsIcon } from "@cds/react/icon";
import { CdsInput } from "@cds/react/input";
import Column from "components/Column";
import Row from "components/Row";
import { isEmpty } from "lodash";
import { useEffect, useState } from "react";
import { validateValuesSchema } from "shared/schema";
import { IAjvValidateResult, IBasicFormParam } from "shared/types";
import { basicFormsDebounceTime, getStringValue } from "shared/utils";

export interface IObjectParamProps {
  id: string;
  label: string;
  param: IBasicFormParam;           // здесь schema.type === "object"
  handleBasicFormParamChange: (
    p: IBasicFormParam,
  ) => (e: React.FormEvent<HTMLInputElement>) => void;
}

/** Одно KV-поле */
interface IEntry {
  k: string;
  v: string;
}

export default function ObjectParam({
  id,
  label,
  param,
  handleBasicFormParamChange,
}: IObjectParamProps) {
  // превращаем {foo:"bar"} -> [{k:"foo",v:"bar"}]
  const [entries, setEntries] = useState<IEntry[]>(() => {
    try {
      const obj = param.currentValue ?? {};
      return Object.keys(obj).map(k => ({ k, v: String(obj[k]) }));
    } catch {
      return [];
    }
  });

  const [validated, setValidated] = useState<IAjvValidateResult>();
  const [timeout, setThisTimeout] = useState<NodeJS.Timeout | null>(null);

  // ――――――――――――――― helpers ――――――――――――――――
  const toObject = () =>
    entries.reduce<Record<string, string>>((acc, { k, v }) => {
      if (k) acc[k] = v;
      return acc;
    }, {});

  const propagate = () => {
    clearTimeout(timeout as NodeJS.Timeout);
    const fn = handleBasicFormParamChange(param);

    const targetCopy = {
      currentTarget: {
        value: getStringValue(toObject()),
        type: "object",
      },
    } as React.FormEvent<HTMLInputElement>;

    setThisTimeout(setTimeout(() => fn(targetCopy), basicFormsDebounceTime));
  };

  // ――――――――――――――― actions ――――――――――――――――
  const addEntry = () => {
    setEntries([...entries, { k: "", v: "" }]);
  };

  const removeEntry = (idx: number) => {
    entries.splice(idx, 1);
    setEntries([...entries]);
  };

  const onFieldChange =
    (idx: number, field: "k" | "v") =>
    (e: React.FormEvent<HTMLInputElement>) => {
      entries[idx] = { ...entries[idx], [field]: e.currentTarget.value };
      setEntries([...entries]);

      // ajv + native HTML валидации
      setValidated(validateValuesSchema(toObject(), param.schema));
      e.currentTarget.reportValidity();

      propagate();
    };

  // ――――――――――――――― UI ――――――――――――――――
  const ctrlMsg =
    !validated?.valid && !isEmpty(validated?.errors) ? (
      <CdsControlMessage status="error">
        {validated?.errors?.map(e => e.message).join(", ")}
      </CdsControlMessage>
    ) : null;

  return (
    <>
      <CdsButton
        action="flat"
        size="sm"
        type="button"
        title="Add key/value"
        onClick={addEntry}
        disabled={param.readOnly}
      >
        <CdsIcon shape="plus" size="sm" /> Add pair
      </CdsButton>

      {ctrlMsg}
      {entries.map((entry, idx) => (
        <Row key={`${id}-${idx}`}>
          <Column span={4}>
            <CdsInput layout="horizontal">
              <label>Key</label>
              <input
                id={`${id}-${idx}-key`}
                disabled={param.readOnly}
                required={param.isRequired}
                value={entry.k}
                onChange={onFieldChange(idx, "k")}
              />
            </CdsInput>
          </Column>
          <Column span={5}>
            <CdsInput layout="horizontal">
              <label>Value</label>
              <input
                id={`${id}-${idx}-val`}
                disabled={param.readOnly}
                required={param.isRequired}
                value={entry.v}
                onChange={onFieldChange(idx, "v")}
              />
            </CdsInput>
          </Column>
          <Column span={1} className="self-center">
            <CdsButton
              action="flat"
              size="sm"
              title="Delete"
              type="button"
              onClick={() => removeEntry(idx)}
              disabled={param.readOnly}
            >
              <CdsIcon shape="minus" size="sm" />
            </CdsButton>
          </Column>
        </Row>
      ))}
    </>
  );
}
