import type { useTranslations } from "next-intl";

import type { NewAssetType } from "@/lib/types";

type Translate = ReturnType<typeof useTranslations>;

// Catalog keys (namespace `common`), not display copy: this module cannot call hooks, so
// each exported label helper takes the component's translator. Entries use the `labelKey`
// property form so the catalog checker can statically verify every key (Ruling 32).
// Unknown runtime values fall back to the raw value, as the old label maps did.
const ASSET_TYPE_LABEL_KEYS: Record<NewAssetType, { labelKey: string }> = {
  app: { labelKey: "taskAssetTypeApp" },
  endpoint: { labelKey: "taskAssetTypeEndpoint" },
  ip: { labelKey: "taskAssetTypeIp" },
  root_domain: { labelKey: "taskAssetTypeRootDomain" },
  service: { labelKey: "taskAssetTypeService" },
  subdomain: { labelKey: "taskAssetTypeSubdomain" },
};

const TASK_ASSET_SOURCE_LABEL_KEYS: Record<string, { labelKey: string }> = {
  agent: { labelKey: "taskAssetSourceAgent" },
  anchor: { labelKey: "taskAssetSourceAnchor" },
  api: { labelKey: "taskAssetSourceApi" },
  company: { labelKey: "taskAssetSourceCompany" },
  legacy: { labelKey: "taskAssetSourceLegacy" },
  manual: { labelKey: "taskAssetSourceManual" },
  system: { labelKey: "taskAssetSourceSystem" },
  task: { labelKey: "taskAssetSourceTask" },
};

export function taskAssetTypeLabel(type: NewAssetType, t: Translate): string {
  const key = ASSET_TYPE_LABEL_KEYS[type]?.labelKey;
  return key ? t(key) : type;
}

export function taskAssetSourceLabel(source: string, t: Translate): string {
  const key = TASK_ASSET_SOURCE_LABEL_KEYS[source]?.labelKey;
  return key ? t(key) : source;
}
