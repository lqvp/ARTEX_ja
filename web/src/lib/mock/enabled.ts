// Mock switch. A build-time injected public variable (only the NEXT_PUBLIC_
// prefix is readable in the browser).
// Set NEXT_PUBLIC_MOCK=1 on Vercel and the whole site runs on mock data, no
// backend required.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
