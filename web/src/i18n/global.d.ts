// `export {}` makes this file a module. Without it the file is a global script,
// and `declare module "next-intl"` becomes an ambient module declaration that
// *shadows* the real package instead of augmenting it — every `import { … } from
// "next-intl"` then fails with TS2305. The idiom is required for any
// `declare module` augmentation to work.
export {};

declare module "next-intl" {
  interface AppConfig {
    Locale: "ja";
  }
}
