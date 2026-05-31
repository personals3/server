// Module-scoped pub/sub for the current user.
// Avoids prop-drilling and Next.js's no-extra-exports-in-layout rule.

export interface User {
  id: string;
  email: string;
  name: string;
  role: "admin" | "user";
  quotaBytes: number;
  usedBytes: number;
}

type Sub = (u: User | null) => void;

let user: User | null = null;
const subs = new Set<Sub>();

export const userStore = {
  get: () => user,
  set: (u: User | null) => { user = u; subs.forEach((s) => s(u)); },
  subscribe: (cb: Sub) => {
    subs.add(cb);
    cb(user);
    return () => { subs.delete(cb); };  // wrap so the destructor returns void
  },
};
