import { lazy, Suspense, useEffect } from "react";
import Dashboard from "./pages/Dashboard";
import Login from "./pages/Login";
import Users from "./pages/Users";
import Audit from "./pages/Audit";
import Account from "./pages/Account";
import { useAuth } from "./auth-context";
import { NavLink } from "./navigation";
import { useLocation, useNavigate } from "./navigation-context";
import LudockMark from "./components/LudockMark";
import PageBoundary from "./components/PageBoundary";
import ViewPreferencesProvider from "./ViewPreferences";

const Console = lazy(() => import("./pages/Console"));
const Diagnostics = lazy(() => import("./pages/Diagnostics"));
const ServerDetail = lazy(() => import("./pages/ServerDetail"));
const Settings = lazy(() => import("./pages/Settings"));
const Files = lazy(() => import("./pages/Files"));
const ApplicationLogs = lazy(() => import("./pages/ApplicationLogs"));

function PageFallback() {
  return (
    <div className="loading-spinner loading-spinner--page" role="status">
      <div className="loading-spinner__ring" aria-hidden="true" />
      <span className="sr-only">Loading Ludock…</span>
    </div>
  );
}

// Keep page selection and its access requirement together so a new page cannot
// accidentally bypass the redirect policy or be omitted from known routes.
const pages = [
  { path: "/", element: <Dashboard /> },
  { path: "/account", element: <Account /> },
  { path: "/users", element: <Users />, adminOnly: true },
  { path: "/audit", element: <Audit />, adminOnly: true },
  { path: "/logs", element: <ApplicationLogs />, adminOnly: true },
  { path: "/diagnostics", element: <Diagnostics />, adminOnly: true },
  { path: "/settings", element: <Settings />, adminOnly: true },
];

const serverPages = [
  { prefix: "servers", render: (id: string) => <ServerDetail key={id} serverId={id} /> },
  { prefix: "files", render: (id: string) => <Files containerId={id} /> },
  { prefix: "console", render: (id: string) => <Console containerId={id} />, console: true },
];

function resolvePage(pathname: string) {
  const page = pages.find((candidate) => candidate.path === pathname);
  if (page) return { ...page, serverTools: false, console: false };
  const match = pathname.match(/^\/([^/]+)\/([^/]+)$/);
  const serverPage = serverPages.find((candidate) => candidate.prefix === match?.[1]);
  if (!match || !serverPage) return null;
  try {
    const id = decodeURIComponent(match[2]);
    return {
      element: serverPage.render(id),
      adminOnly: false,
      serverTools: !serverPage.console,
      console: Boolean(serverPage.console),
    };
  } catch {
    return null;
  }
}

function App() {
  const { loading, statusError, authenticated, user, refreshStatus, logout } =
    useAuth();
  const location = useLocation();
  const navigate = useNavigate();
  const route = resolvePage(location.pathname);
  const allowed = route !== null && (!route.adminOnly || user?.role === "admin");
  const isConsolePage = Boolean(route?.console);

  useEffect(() => {
    if (authenticated && !allowed) {
      navigate("/", { replace: true });
    }
  }, [authenticated, allowed, location.pathname, navigate]);

  if (loading) {
    return <PageFallback />;
  }

  if (statusError) {
    return (
      <main className="login-page">
        <section
          className="login-card"
          aria-labelledby="connection-error-title"
        >
          <LudockMark className="sidebar__logo-icon login-card__logo" />
          <h1 className="login-card__title" id="connection-error-title">
            Unable to reach Ludock
          </h1>
          <p className="login-card__description">
            The panel could not confirm your session or setup state.
            Check that the backend is running, then try again.
          </p>
          <button
            className="login-card__submit"
            type="button"
            onClick={() => void refreshStatus()}
          >
            Try again
          </button>
        </section>
      </main>
    );
  }

  if (!authenticated) {
    return <Login />;
  }

  const page = allowed ? route.element : <Dashboard />;

  return (
    <ViewPreferencesProvider key={user?.id}>
    <div className="app-layout">
      <a className="skip-link" href="#main-content">Skip to content</a>
      {!isConsolePage && (
        <aside className="sidebar">
          <div className="sidebar__header">
            <NavLink className="sidebar__logo" to="/" end aria-label="Ludock servers">
              <LudockMark className="sidebar__logo-icon" />
              <div className="sidebar__logo-text">Ludock</div>
            </NavLink>
          </div>
          <nav className="sidebar__nav" aria-label="Primary navigation">
            <NavLink
              to="/"
              end
              className={`nav-link ${route?.serverTools ? "nav-link--active" : ""}`}
              aria-current={route?.serverTools ? "location" : undefined}
            >
              Servers
            </NavLink>
            {user?.role === "admin" && (
              <>
                <div className="sidebar__group" role="group" aria-labelledby="administration-label">
                  <p className="sidebar__group-label" id="administration-label">Administration</p>
                  <NavLink to="/users" className="nav-link">Users</NavLink>
                  <NavLink to="/settings" className="nav-link">Settings</NavLink>
                  <NavLink to="/diagnostics" className="nav-link">Diagnostics</NavLink>
                </div>
                <div className="sidebar__group" role="group" aria-labelledby="history-label">
                  <p className="sidebar__group-label" id="history-label">History</p>
                  <NavLink to="/audit" className="nav-link">Audit log</NavLink>
                  <NavLink to="/logs" className="nav-link">Ludock logs</NavLink>
                </div>
              </>
            )}
          </nav>
          <div className="sidebar__footer">
            <NavLink to="/account" className="nav-link sidebar__account">Account</NavLink>
            <div className="sidebar__session">
              <div className="sidebar__identity">
                <span className="sidebar__username" title={user?.username}>{user?.username}</span>
                <span className="sidebar__role">{user?.role === "admin" ? "Administrator" : user?.role}</span>
              </div>
              <button className="sidebar__logout" onClick={logout}>Sign out</button>
            </div>
          </div>
        </aside>
      )}
      {!isConsolePage && (
        <header className="mobile-header">
          <NavLink className="sidebar__logo" to="/" end aria-label="Ludock servers">
            <LudockMark className="sidebar__logo-icon" />
            <div className="sidebar__logo-text">Ludock</div>
          </NavLink>
          <span className="mobile-header__user">{user?.username}</span>
          <button className="mobile-header__logout" onClick={logout}>
            Sign out
          </button>
        </header>
      )}
      {!isConsolePage && (
        <nav className="mobile-nav" aria-label="Primary navigation">
          <NavLink to="/" end className={route?.serverTools ? "nav-link--active" : undefined} aria-current={route?.serverTools ? "location" : undefined}>
            Servers
          </NavLink>
          <NavLink to="/account">Account</NavLink>
          {user?.role === "admin" && <NavLink to="/settings">Settings</NavLink>}
          {user?.role === "admin" && (
            <NavLink to="/diagnostics">Diagnostics</NavLink>
          )}
          {user?.role === "admin" && <NavLink to="/users">Users</NavLink>}
          {user?.role === "admin" && <NavLink to="/audit">Audit</NavLink>}
          {user?.role === "admin" && <NavLink to="/logs">Logs</NavLink>}
        </nav>
      )}
      <main
        id="main-content"
        tabIndex={-1}
        className={`main-content ${isConsolePage ? "main-content--console" : ""}`}
      >
        <PageBoundary key={location.pathname}>
          <Suspense fallback={<PageFallback />}>{page}</Suspense>
        </PageBoundary>
      </main>
    </div>
    </ViewPreferencesProvider>
  );
}

export default App;
