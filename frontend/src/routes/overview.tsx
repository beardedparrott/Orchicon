import { createFileRoute, redirect } from "@tanstack/react-router";
export const Route = createFileRoute("/overview")({ beforeLoad: () => { throw redirect({ to: "/dashboard" }); }, component: () => null });
