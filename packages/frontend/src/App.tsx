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

const Console = lazy(() => import("./pages/Console"));
const Diagnostics = lazy(() => import("./pages/Diagnostics"));
const ServerDetail = lazy(() => import("./pages/ServerDetail"));
const Settings = lazy(() => import("./pages/Settings"));
const Files = lazy(() => import("./pages/Files"));
const ApplicationLogs = lazy(() => import("./pages/ApplicationLogs"));

function PageFallback() {
  return (
    <div className="loading-spinner loading-spinner--page">
      <div className="loading-spinner__ring" />
    </div>
  );
}

function routeParameter(match: RegExpMatchArray | null): string | null {
  if (!match?.[1]) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return null;
  }
}

function App() {
  const { loading, statusError, authenticated, user, refreshStatus, logout } =
    useAuth();
  const location = useLocation();
  const navigate = useNavigate();
  const serverMatch = location.pathname.match(/^\/servers\/([^/]+)$/);
  const serverId = routeParameter(serverMatch);
  const consoleMatch = location.pathname.match(/^\/console\/([^/]+)$/);
  const filesMatch = location.pathname.match(/^\/files\/([^/]+)$/);
  const consoleId = routeParameter(consoleMatch);
  const filesId = routeParameter(filesMatch);
  const isConsolePage = consoleId !== null;
  const knownPath =
    location.pathname === "/" ||
    location.pathname === "/account" ||
    location.pathname === "/users" ||
    location.pathname === "/audit" ||
    location.pathname === "/logs" ||
    location.pathname === "/settings" ||
    location.pathname === "/diagnostics" ||
    serverId !== null ||
    filesId !== null ||
    isConsolePage;

  useEffect(() => {
    if (
      authenticated &&
      (!knownPath ||
        ((location.pathname === "/users" ||
          location.pathname === "/audit" ||
          location.pathname === "/logs" ||
          location.pathname === "/settings" ||
          location.pathname === "/diagnostics") &&
          user?.role !== "admin"))
    ) {
      navigate("/", { replace: true });
    }
  }, [authenticated, knownPath, location.pathname, navigate, user?.role]);

  if (loading) {
    return (
      <div className="loading-spinner loading-spinner--page">
        <div className="loading-spinner__ring" />
      </div>
    );
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
            The panel could not determine whether initial setup is required.
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

  let page = <Dashboard />;
  if (location.pathname === "/account") {
    page = <Account />;
  } else if (location.pathname === "/users" && user?.role === "admin") {
    page = <Users />;
  } else if (location.pathname === "/audit" && user?.role === "admin") {
    page = <Audit />;
  } else if (location.pathname === "/logs" && user?.role === "admin") {
    page = (
      <Suspense fallback={<PageFallback />}>
        <ApplicationLogs />
      </Suspense>
    );
  } else if (location.pathname === "/diagnostics" && user?.role === "admin") {
    page = (
      <Suspense fallback={<PageFallback />}>
        <Diagnostics />
      </Suspense>
    );
  } else if (location.pathname === "/settings" && user?.role === "admin") {
    page = (
      <Suspense fallback={<PageFallback />}>
        <Settings />
      </Suspense>
    );
  } else if (serverId) {
    page = (
      <Suspense fallback={<PageFallback />}>
        <ServerDetail key={serverId} serverId={serverId} />
      </Suspense>
    );
  } else if (consoleId) {
    page = (
      <Suspense fallback={<PageFallback />}>
        <Console containerId={consoleId} />
      </Suspense>
    );
  } else if (filesId) {
    page = (
      <Suspense fallback={<PageFallback />}>
        <Files containerId={filesId} />
      </Suspense>
    );
  }

  return (
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
              className={`nav-link ${serverId || filesId ? "nav-link--active" : ""}`}
              aria-current={serverId || filesId ? "location" : undefined}
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
          <NavLink to="/" end className={serverId || filesId ? "nav-link--active" : undefined} aria-current={serverId || filesId ? "location" : undefined}>
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
        {page}
      </main>
    </div>
  );
}

export default App;
