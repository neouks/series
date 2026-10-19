"use client";

import { SidebarTrigger } from "@/components/ui/sidebar";
import { usePathname } from "next/navigation";
import { type ReactNode } from "react";

export function MainContent({ children }: { children: ReactNode }) {
  const pathname = usePathname().replace(/\/+$/, "");
  if (pathname === "/function/tasks/detail") return <>{children}</>;
  return (
    <>
      <div className="flex h-12 shrink-0 items-center px-4 md:hidden">
        <SidebarTrigger aria-label="打开导航" />
      </div>
      <main className="workspace-page min-h-0 min-w-0 flex-1 has-data-[content-padding=false]:p-0">{children}</main>
    </>
  );
}
