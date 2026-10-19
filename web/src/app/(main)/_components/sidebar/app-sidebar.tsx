"use client";

import { useSettings } from "@/components/settings/settings-context";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarTrigger,
  useSidebar,
} from "@/components/ui/sidebar";
import { useCurrentUser } from "@/hooks/use-current-user";
import { sidebarItems } from "@/navigation/sidebar/sidebar-items";
import { Palette, Settings2 } from "lucide-react";
import { NavMain } from "./nav-main";
import { NavUser } from "./nav-user";

export function AppSidebar(props: React.ComponentProps<typeof Sidebar>) {
  const currentUser = useCurrentUser();
  const settings = useSettings();
  const { setOpenMobile } = useSidebar();
  return (
    <Sidebar {...props} variant="sidebar" collapsible="icon">
      <SidebarHeader className="items-end px-3 py-2">
        <SidebarTrigger aria-label="折叠侧栏" />
      </SidebarHeader>
      <SidebarContent className="gap-1 px-2">
        <NavMain items={sidebarItems} />
      </SidebarContent>
      <SidebarFooter className="gap-1 border-t p-3">
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              tooltip="设置"
              onClick={() => {
                setOpenMobile(false);
                settings.open();
              }}
            >
              <Settings2 />
              <span>设置</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton
              tooltip="外观"
              onClick={() => {
                setOpenMobile(false);
                settings.open("appearance");
              }}
            >
              <Palette />
              <span>外观</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
        <NavUser user={currentUser} />
      </SidebarFooter>
    </Sidebar>
  );
}
