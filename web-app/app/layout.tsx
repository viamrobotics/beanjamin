import "./globals.css";
import type { Metadata } from "next";
import { GeistSans } from "geist/font/sans";
import { GeistMono } from "geist/font/mono";
import { Just_Me_Again_Down_Here } from "next/font/google";

const justMeAgainDownHere = Just_Me_Again_Down_Here({
  weight: "400",
  subsets: ["latin"],
  variable: "--font-just-me",
  display: "swap",
});

export const metadata: Metadata = {
  title: "Barista Misspell",
  description: "How would a barista spell your name?",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" className={`${GeistSans.variable} ${GeistMono.variable} ${justMeAgainDownHere.variable}`}>
      <body className="m-0 bg-white">
        {children}
      </body>
    </html>
  );
}