"use client";

import * as React from "react";

import { CheckIcon, CopyIcon } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { cn, copyText } from "@/lib/utils";

type CopyButtonProps = {
  // Text to copy; an empty value disables the button.
  text: string | null | undefined;
  // Toast copy after a successful copy; falls back to common.copied when omitted.
  successMessage?: string;
  label?: React.ReactNode;
  size?: React.ComponentProps<typeof Button>["size"];
  variant?: React.ComponentProps<typeof Button>["variant"];
  className?: string;
};

// Shared "copy to clipboard" button with built-in success/failure feedback; it
// degrades automatically on insecure HTTP contexts (see copyText).
export function CopyButton({
  text,
  successMessage,
  label,
  size = "sm",
  variant = "outline",
  className,
}: CopyButtonProps) {
  const t = useTranslations("components");
  const tc = useTranslations("common");
  const [copied, setCopied] = React.useState(false);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  React.useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  async function handleCopy() {
    if (!text) return;
    const ok = await copyText(text);
    if (ok) {
      setCopied(true);
      toast.success(successMessage ?? tc("copied"));
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => setCopied(false), 1500);
    } else {
      toast.error(t("copyFailManual"));
    }
  }

  return (
    <Button
      type="button"
      size={size}
      variant={variant}
      className={cn(className)}
      disabled={!text}
      onClick={handleCopy}
    >
      {copied ? <CheckIcon /> : <CopyIcon />}
      {label ?? tc("copy")}
    </Button>
  );
}
