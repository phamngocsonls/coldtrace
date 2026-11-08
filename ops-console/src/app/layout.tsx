import type { Metadata } from "next";
import { Fira_Code, Fira_Sans } from "next/font/google";
import "./globals.css";
import { Providers } from "@/lib/live";
import { Nav } from "@/components/Nav";
import { AiAgentPanel } from "@/components/AiAgentPanel";

const firaSans = Fira_Sans({
  variable: "--font-fira-sans",
  subsets: ["latin"],
  weight: ["300", "400", "500", "600", "700"],
});

const firaCode = Fira_Code({
  variable: "--font-fira-code",
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
});

export const metadata: Metadata = {
  title: "ColdTrace Ops",
  description: "Cold-chain IoT operations console — fleet, AI decisions, commands, compliance",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en" className={`${firaSans.variable} ${firaCode.variable} h-full antialiased`}>
      <body className="min-h-full flex flex-col">
        <Providers>
          <Nav />
          <main className="flex-1 p-4">{children}</main>
          <AiAgentPanel />
        </Providers>
      </body>
    </html>
  );
}
