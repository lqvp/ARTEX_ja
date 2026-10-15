"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { api } from "@/lib/api";

/**
 * The "new version available" hint in the top bar: it queries once per full page
 * load and lights up next to the version number when an update exists; clicking
 * it goes straight to the "Version & Updates" card on the system settings page.
 *
 * The backend caches its GitHub lookup for 30 minutes, so querying on every mount
 * is safe — the unauthenticated GitHub API allows only 60 requests/hour/IP, and
 * without that cache a few extra tabs would burn the whole quota, after which a
 * genuinely wanted update could no longer be checked.
 *
 * Query failures are always silent: the top bar is not the place for errors, and
 * the user sees the reason when they click "Check for updates" in the settings page.
 */
export function UpdateBadge() {
  const t = useTranslations("components");
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already encodes the "version is comparable" judgement, so dev
        // builds never light this hint up.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silent: no network / GitHub rate limiting must not pop a top-bar error.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={t("updateBadgeTitle", { version: latest })}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Pulsing dot: the top bar holds many elements, so plain text is easily
          missed — the animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">{t("updateBadgeLabel", { version: latest })}</span>
      <span className="sm:hidden">{t("updateBadgeShort")}</span>
    </Link>
  );
}
