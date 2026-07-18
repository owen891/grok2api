import { useTranslation } from "react-i18next";

import { EgressGroups } from "@/features/settings/egress-groups";
import { EgressNodes } from "@/features/settings/egress-nodes";

export function ProxiesPage() {
  const { t } = useTranslation();

  return (
    <div className="w-full space-y-8">
      <header>
        <h1 className="text-xl font-medium">{t("nav.proxy")}</h1>
        <p className="mt-1 text-xs text-muted-foreground">{t("settings.egressGroups.description")}</p>
      </header>
      <section className="space-y-4">
        <h2 className="text-sm font-medium">{t("settings.egress.title")}</h2>
        <EgressNodes />
      </section>
      <section className="space-y-4">
        <h2 className="text-sm font-medium">{t("settings.egressGroups.title")}</h2>
        <EgressGroups />
      </section>
    </div>
  );
}
