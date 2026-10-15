"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// Send/newline key bindings for the chat composer. A pure client-side preference: it only
// lands in localStorage — never in the database and never synced per account — so switching
// browsers requires re-setting it. See issue #39: 0.3.2 changed the send key from Ctrl+Enter
// to Enter; this restores the old binding as an option.
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

// Labels for these options live in the catalog under settings.sendMode.* and are rendered
// by the settings page; the entries here only carry the value.
export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode }[] = [{ value: "enter" }, { value: "ctrl-enter" }];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// Subscribers within the same tab. The localStorage "storage" event only fires in OTHER
// tabs, so after a settings change on this page the composer is notified via emit —
// otherwise a refresh would be required.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// Returns a string literal, so Object.is compares by value and useSyncExternalStore
// cannot loop.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// No localStorage on the server: render the default first, then getSnapshot corrects it
// after hydration.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey decides whether a keypress should submit the composer.
// isComposing / keyCode 229 mean an IME (Chinese etc.) is mid-composition and must pass
// through, or Enter would accidentally send while committing a word.
// "enter" mode only excludes Shift, matching 0.3.2 behavior verbatim — users who never
// changed the setting keep the same feel.
// "ctrl-enter" accepts both Ctrl and Cmd (macOS).
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
