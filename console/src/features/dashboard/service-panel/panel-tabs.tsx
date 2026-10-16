import { ChartColumn, Globe, KeyRound, Layers, Settings } from "lucide-react";

export const servicePanelTabs = [
	{ id: "deployments", label: "Deployments", icon: <Layers size={11} /> },
	{ id: "variables", label: "Variables", icon: <KeyRound size={11} /> },
	{ id: "domains", label: "Domains", icon: <Globe size={11} /> },
	{ id: "usage", label: "Usage", icon: <ChartColumn size={11} /> },
	{ id: "settings", label: "Settings", icon: <Settings size={11} /> },
] as const;
