"use client";

import * as React from "react";

import { useRouter } from "next/navigation";

import { Search } from "lucide-react";
import { useTranslations } from "next-intl";

import { Button } from "@/components/ui/button";
import {
  Command,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";
import type { NavGroup, NavMainItem } from "@/navigation/sidebar/sidebar-items";
import { sidebarItems } from "@/navigation/sidebar/sidebar-items";

type SearchItem = {
  id: string;
  group: string;
  label: string;
  url: string;
  icon?: NavMainItem["icon"];
  disabled?: boolean;
  newTab?: boolean;
};

function getAvailableItems(items: SearchItem[]) {
  return items.filter((item) => !item.disabled && !item.url.includes("coming-soon"));
}

function groupBy(items: SearchItem[]) {
  const groups = [...new Set(items.map((item) => item.group))];
  return groups.map((group) => ({
    group,
    items: items.filter((item) => item.group === group),
  }));
}

export function SearchDialog() {
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState("");
  const router = useRouter();
  const t = useTranslations("nav");
  const tc = useTranslations("common");

  // sidebarItems only stores catalog keys, so the translated group/item labels used
  // for search and display are derived here; useMemo keeps the flatMap from rerunning
  // on every keystroke.
  const { searchItems, recommendations } = React.useMemo(() => {
    const groupLabelOf = (group: NavGroup) => (group.labelKey ? t(group.labelKey) : undefined);
    const sidebarGroupLabels = new Set(sidebarItems.flatMap((group) => (group.labelKey ? [t(group.labelKey)] : [])));
    const getSubItemGroup = (groupLabel: string | undefined, itemTitle: string) =>
      sidebarGroupLabels.has(itemTitle) ? (groupLabel ?? t("other")) : itemTitle;

    const allItems: SearchItem[] = sidebarItems.flatMap((group) =>
      group.items.flatMap((item) => {
        const groupLabel = groupLabelOf(group);
        const itemTitle = t(item.titleKey);
        if (item.subItems) {
          return item.subItems.map((sub) => ({
            id: sub.id,
            group: getSubItemGroup(groupLabel, itemTitle),
            label: t(sub.titleKey),
            url: sub.url,
            icon: item.icon,
            disabled: sub.disabled,
            newTab: sub.newTab,
          }));
        }
        return [
          {
            id: item.id,
            group: groupLabel ?? t("other"),
            label: itemTitle,
            url: item.url,
            icon: item.icon,
            disabled: item.disabled,
            newTab: item.newTab,
          },
        ];
      }),
    );

    return { searchItems: allItems, recommendations: getAvailableItems(allItems) };
  }, [t]);

  React.useEffect(() => {
    const down = (e: KeyboardEvent) => {
      if (e.key === "j" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen((prev) => !prev);
      }
    };
    document.addEventListener("keydown", down);
    return () => document.removeEventListener("keydown", down);
  }, []);

  const handleOpenChange = (value: boolean) => {
    setOpen(value);
    if (!value) setQuery("");
  };

  const handleSelect = (item: SearchItem) => {
    if (item.disabled) return;
    handleOpenChange(false);
    if (item.newTab) {
      window.open(item.url, "_blank", "noopener,noreferrer");
    } else {
      router.push(item.url);
    }
  };

  const renderGroups = (items: SearchItem[]) =>
    groupBy(items).map(({ group, items: groupItems }, index) => (
      <React.Fragment key={group}>
        {index > 0 && <CommandSeparator />}
        <CommandGroup heading={group}>
          {groupItems.map((item) => (
            <CommandItem
              disabled={item.disabled}
              key={`${group}-${item.id}`}
              value={`${item.group} ${item.label}`}
              onSelect={() => handleSelect(item)}
            >
              <span className="flex min-w-0 items-center gap-2">
                {item.icon && <item.icon />}
                <span className="truncate">{item.label}</span>
              </span>
            </CommandItem>
          ))}
        </CommandGroup>
      </React.Fragment>
    ));

  return (
    <>
      <Button
        onClick={() => handleOpenChange(true)}
        variant="link"
        className="px-0! font-normal text-muted-foreground hover:no-underline"
      >
        <Search data-icon="inline-start" />
        {tc("search")}
        <kbd className="inline-flex h-5 select-none items-center gap-1 rounded border bg-muted px-1.5 font-medium text-[10px]">
          <span className="text-xs">⌘</span>J
        </kbd>
      </Button>
      <CommandDialog open={open} onOpenChange={handleOpenChange}>
        <Command>
          <CommandInput placeholder={t("searchDialogPlaceholder")} value={query} onValueChange={setQuery} />
          <CommandList>
            <CommandEmpty>{t("searchEmpty")}</CommandEmpty>
            {query ? renderGroups(searchItems) : renderGroups(recommendations)}
          </CommandList>
        </Command>
      </CommandDialog>
    </>
  );
}
