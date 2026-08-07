import { lazy, Suspense, useEffect } from "react";
import Dashboard from "./pages/Dashboard";
import Login from "./pages/Login";
import Users from "./pages/Users";
import Audit from "./pages/Audit";
import Account from "./pages/Account";
import { useAuth } from "./auth-context";
import { NavLink } from "./navigation";
import { useLocation, useNavigate } from "./navigation-context";

const Console = lazy(() => import("./pages/Console"));
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
  const {
    loading,
    statusError,
    authenticated,
    user,
    refreshStatus,
    logout,
  } = useAuth();
  const location = useLocation();
  const navigate = useNavigate();
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
    filesId !== null ||
    isConsolePage;

  useEffect(() => {
    if (
      authenticated &&
      (!knownPath ||
        ((location.pathname === "/users" ||
          location.pathname === "/audit" ||
          location.pathname === "/logs") &&
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
        <section className="login-card" aria-labelledby="connection-error-title">
          <div className="sidebar__logo-icon login-card__logo">LU</div>
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
      {!isConsolePage && (
        <aside className="sidebar">
          <div className="sidebar__header">
            <div className="sidebar__logo">
              <div className="sidebar__logo-icon">LU</div>
              <div className="sidebar__logo-text">Ludock</div>
            </div>
          </div>
          <nav className="sidebar__nav">
            <NavLink
              to="/"
              end
              className={({ isActive }) =>
                `nav-link ${isActive ? "nav-link--active" : ""}`
              }
            >
              Servers
            </NavLink>
            <NavLink
              to="/account"
              className={({ isActive }) =>
                `nav-link ${isActive ? "nav-link--active" : ""}`
              }
            >
              Account
            </NavLink>
            {user?.role === "admin" && (
              <>
                <NavLink
                  to="/users"
                  className={({ isActive }) =>
                    `nav-link ${isActive ? "nav-link--active" : ""}`
                  }
                >
                  Users
                </NavLink>
                <NavLink
                  to="/audit"
                  className={({ isActive }) =>
                    `nav-link ${isActive ? "nav-link--active" : ""}`
                  }
                >
                  Audit log
                </NavLink>
                <NavLink
                  to="/logs"
                  className={({ isActive }) =>
                    `nav-link ${isActive ? "nav-link--active" : ""}`
                  }
                >
                  Ludock logs
                </NavLink>
              </>
            )}
          </nav>
          <div className="sidebar__footer">
            <span title={user?.role}>{user?.username}</span>
            <button className="sidebar__logout" onClick={logout}>
              Sign out
            </button>
          </div>
        </aside>
      )}
      {!isConsolePage && (
        <header className="mobile-header">
          <div className="sidebar__logo">
            <div className="sidebar__logo-icon">LU</div>
            <div className="sidebar__logo-text">Ludock</div>
          </div>
          <span className="mobile-header__user">{user?.username}</span>
          <button className="mobile-header__logout" onClick={logout}>
            Sign out
          </button>
        </header>
      )}
      {!isConsolePage && (
        <nav className="mobile-nav" aria-label="Primary navigation">
          <NavLink to="/" end>
            Servers
          </NavLink>
          <NavLink to="/account">Account</NavLink>
          {user?.role === "admin" && <NavLink to="/users">Users</NavLink>}
          {user?.role === "admin" && <NavLink to="/audit">Audit</NavLink>}
          {user?.role === "admin" && <NavLink to="/logs">Logs</NavLink>}
        </nav>
      )}
      <main
        className={`main-content ${isConsolePage ? "main-content--console" : ""}`}
      >
        {page}
      </main>
    </div>
  );
}

export default App;
