interface Props {
  loading: boolean;
  nextCursor: string | null | undefined;
  hasCursor: boolean;
  onOlder: (cursor: string) => void;
  onNewer: () => void;
}

export default function HistoryPagination({ loading, nextCursor, hasCursor, onOlder, onNewer }: Props) {
  return (
    <nav className="history-pagination" aria-label="History pages">
      <button className="secondary-btn" disabled={loading || !hasCursor} onClick={onNewer}>Newer</button>
      <span className="section-note">Newest first · up to 50 per page</span>
      <button className="secondary-btn" disabled={loading || !nextCursor} onClick={() => { if (nextCursor) onOlder(nextCursor); }}>Older</button>
    </nav>
  );
}
