import { redirect } from "next/navigation";

export default function Home() {
  redirect("/function/tasks/new");
  return <>Coming Soon</>;
}
