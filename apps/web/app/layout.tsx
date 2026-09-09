export const metadata = {
  title: "OpenLane",
  description: "The open-source system of execution for post-sale delivery.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
