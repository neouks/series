"use client";
import { useSettings } from "@/components/settings/settings-context";
import { Button } from "@/components/ui/button";
import { Settings } from "lucide-react";
export function LayoutControls() {
  const settings = useSettings();
  return (
    <Button variant="ghost" size="icon" aria-label="外观设置" onClick={() => settings.open("appearance")}>
      <Settings />
    </Button>
  );
}
