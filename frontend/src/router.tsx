import { useEffect, useState, type MouseEvent, type ReactNode } from "react";

// A tiny router: two pages don't justify a routing library. The load
// balancer (and Vite in dev) serve index.html for every unknown path, so
// /dashboard works on reload and when shared as a link.

const CHANGE = "app:navigate";

export function usePath(): string {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const on = () => setPath(window.location.pathname);
    window.addEventListener("popstate", on);
    window.addEventListener(CHANGE, on);
    return () => {
      window.removeEventListener("popstate", on);
      window.removeEventListener(CHANGE, on);
    };
  }, []);
  return path;
}

export function navigate(to: string) {
  window.history.pushState(null, "", to);
  window.dispatchEvent(new Event(CHANGE));
  const hash = to.split("#")[1];
  // Let the new page render, then jump to the anchor (or the top).
  requestAnimationFrame(() => {
    const el = hash ? document.getElementById(hash) : null;
    if (el) el.scrollIntoView();
    else window.scrollTo(0, 0);
  });
}

export function Link({ to, className, children }: { to: string; className?: string; children: ReactNode }) {
  const onClick = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  };
  return (
    <a href={to} className={className} onClick={onClick}>
      {children}
    </a>
  );
}
