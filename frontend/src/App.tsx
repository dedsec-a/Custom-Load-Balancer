import { useEffect } from "react";
import Dashboard from "./Dashboard";
import Experiments from "./Experiments";
import Landing from "./Landing";
import { usePath } from "./router";

export default function App() {
  const path = usePath();
  const page = path.startsWith("/dashboard") ? "dashboard" : path.startsWith("/experiments") ? "experiments" : "landing";

  useEffect(() => {
    document.title =
      page === "dashboard" ? "Dashboard · Load Balancer" : page === "experiments" ? "Experiments · Load Balancer" : "Load Balancer, Live";
  }, [page]);

  if (page === "dashboard") return <Dashboard />;
  if (page === "experiments") return <Experiments />;
  return <Landing />;
}
