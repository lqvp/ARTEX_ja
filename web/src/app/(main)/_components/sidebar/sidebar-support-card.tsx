import Link from "next/link";

import { useTranslations } from "next-intl";
import { siX } from "simple-icons";

import { SimpleIcon } from "@/components/simple-icon";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

export function SidebarSupportCard() {
  const t = useTranslations("components");

  return (
    <Card size="sm" className="overflow-hidden shadow-none group-data-[collapsible=icon]:hidden">
      <CardHeader className="min-w-0 px-4">
        <CardTitle className="truncate text-sm">{t("supportTitle")}</CardTitle>
        <CardDescription className="line-clamp-2">
          {t.rich("supportDescription", {
            xLink: (chunks) => (
              <Link
                href="https://x.com/arhamkhnz"
                target="_blank"
                rel="noreferrer"
                aria-label={t("supportReachOutAria")}
                className="inline-flex items-center text-foreground"
              >
                {chunks}
                <SimpleIcon icon={siX} aria-hidden className="size-3 fill-current" />
              </Link>
            ),
            reachOutOnX: "X",
          })}
        </CardDescription>
      </CardHeader>
    </Card>
  );
}
