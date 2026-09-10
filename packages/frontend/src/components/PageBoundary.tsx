import { Component, type ReactNode } from "react";
import { NavLink } from "../navigation";

export default class PageBoundary extends Component<
  { children: ReactNode },
  { failed: boolean }
> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  render() {
    if (!this.state.failed) return this.props.children;

    return (
      <section className="page" aria-labelledby="page-error-title">
        <div className="page__header" role="alert">
          <h1 className="page__title" id="page-error-title">Page unavailable</h1>
          <p className="page__subtitle">
            This page could not be loaded. Reload it to try again, or return to Servers.
          </p>
        </div>
        <div className="inline-actions">
          <button type="button" className="primary-btn" onClick={() => window.location.reload()}>
            Reload page
          </button>
          <NavLink to="/" className="secondary-btn">Servers</NavLink>
        </div>
      </section>
    );
  }
}
