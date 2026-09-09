export default function Home() {
  return (
    <main className="flex min-h-screen items-center justify-center">
      <div className="text-center">
        <h1 className="text-4xl font-bold">OpenLane</h1>
        <p className="mt-2 text-gray-600">
          The open-source system of execution for post-sale delivery.
        </p>
        <p className="mt-4 text-sm text-gray-400">
          P0 scaffold — the product builds from{" "}
          <code>docs/openlane_spec.md</code>.
        </p>
      </div>
    </main>
  );
}
