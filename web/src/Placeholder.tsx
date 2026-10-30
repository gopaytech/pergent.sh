export default function Placeholder({ title }: { title: string }) {
  return (
    <>
      <div className="top">
        <div>
          <h1>{title}</h1>
          <div className="sub">The dashboard is being built.</div>
        </div>
      </div>
      <div className="card empty">Nothing here yet.</div>
    </>
  );
}
