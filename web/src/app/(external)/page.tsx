import { redirect } from "next/navigation";

import { useTranslations } from "next-intl";

export default function Home() {
  const t = useTranslations("common");
  redirect("/function/tasks");
  return <>{t("comingSoon")}</>;
}
