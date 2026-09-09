import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type AnchorHTMLAttributes,
  type MouseEvent,
  type ReactNode,
} from "react";
import { NavigationContext, useNavigation } from "./navigation-context";

export function NavigationProvider({ children }: { children: ReactNode }) {
  const [pathname, setPathname] = useState(window.location.pathname);

  useEffect(() => {
    const handlePopState = () => setPathname(window.location.pathname);
    window.addEventListener("popstate", handlePopState);
    return () => window.removeEventListener("popstate", handlePopState);
  }, []);

  const navigate = useCallback(
    (to: string, options: { replace?: boolean } = {}) => {
      if (to === window.location.pathname) return;
      window.history[options.replace ? "replaceState" : "pushState"]({}, "", to);
      setPathname(window.location.pathname);
      window.scrollTo({ top: 0, behavior: "auto" });
    },
    []
  );

  const value = useMemo(() => ({ pathname, navigate }), [pathname, navigate]);
  return (
    <NavigationContext.Provider value={value}>
      {children}
    </NavigationContext.Provider>
  );
}

interface NavLinkProps
  extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "className" | "href"> {
  to: string;
  end?: boolean;
  className?: string | ((state: { isActive: boolean }) => string);
}

export function NavLink({
  to,
  end = false,
  className,
  onClick,
  ...props
}: NavLinkProps) {
  const { pathname, navigate } = useNavigation();
  const isActive = end
    ? pathname === to
    : pathname === to || pathname.startsWith(`${to}/`);
  const resolvedClassName =
    typeof className === "function" ? className({ isActive }) : className;

  const handleClick = (event: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(event);
    if (
      event.defaultPrevented ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    ) {
      return;
    }
    event.preventDefault();
    navigate(to);
  };

  return (
    <a
      {...props}
      href={to}
      aria-current={pathname === to ? "page" : props["aria-current"]}
      className={`${resolvedClassName || ""}${isActive ? " active" : ""}`.trim()}
      onClick={handleClick}
    />
  );
}
