import { Routes, Route, NavLink, useLocation } from "react-router-dom";
import Dashboard from "./pages/Dashboard";
import Console from "./pages/Console";

function App() {
  const location = useLocation();
  const isConsolePage = location.pathname.startsWith("/console/");

  return (
    <div className="app-layout">
      {!isConsolePage && (
        <aside className="sidebar">
          <div className="sidebar__header">
            <div className="sidebar__logo">
              <div className="sidebar__logo-icon">GP</div>
              <div>
                <div className="sidebar__logo-text">Game Panel</div>
              </div>
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
              <span className="nav-link__icon">⌂</span>
              Servers
            </NavLink>
          </nav>
          <div className="sidebar__footer">
            v0.1.0
          </div>
        </aside>
      )}
      <main className="main-content" style={isConsolePage ? { marginLeft: 0 } : undefined}>
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/console/:containerId" element={<Console />} />
        </Routes>
      </main>
    </div>
  );
}

export default App;
