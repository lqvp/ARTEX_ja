"use client";

import * as React from "react";
import { useTranslations } from "next-intl";

export function ProbeA({ x }: { x: string | undefined }) {
  const t = useTranslations("components");
  const labels = React.useMemo(() => ({ a: t("kindTask") }), [t]);
  const [v, setV] = React.useState("");
  React.useEffect(() => {
    setV(`${labels.a}-${t("kindBegin")}-${x ?? "none"}`);
  }, [labels, x]);
  return <div>{v}</div>;
}
