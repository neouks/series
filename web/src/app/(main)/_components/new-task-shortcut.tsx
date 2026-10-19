"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useSidebar } from "@/components/ui/sidebar";
import { NEW_TASK_PATH, workspaceShortcut } from "@/lib/workspace-navigation";

export function NewTaskShortcut() {
  const router = useRouter();
  const { setOpenMobile } = useSidebar();

  useEffect(() => {
    const down = (event: KeyboardEvent) => {
      const editable =
        event.target instanceof Element && !!event.target.closest("input, textarea, select, [contenteditable='true']");
      const overlay = !!document.querySelector(
        '[role="dialog"][data-state="open"], [role="alertdialog"][data-state="open"], [data-slot="sheet-content"][data-state="open"]',
      );
      if (!workspaceShortcut(event, editable || overlay)) return;
      event.preventDefault();
      setOpenMobile(false);
      router.push(NEW_TASK_PATH);
    };
    document.addEventListener("keydown", down);
    return () => document.removeEventListener("keydown", down);
  }, [router, setOpenMobile]);

  return null;
}
