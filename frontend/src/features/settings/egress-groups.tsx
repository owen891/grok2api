import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Trash2, Upload } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { createEgressGroup, deleteEgressGroup, importEgressGroup, listEgressGroups, updateEgressGroup, type EgressGroupDTO, type EgressGroupInput, type EgressGroupStrategy, type EgressScope } from "@/features/settings/settings-api";

const empty: EgressGroupInput = { name: "", scope: "grok_build", enabled: true, strategy: "least_load", maxConcurrency: 0 };

export function EgressGroups() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: ["egress-groups"], queryFn: () => listEgressGroups() });
  const [editing, setEditing] = useState<EgressGroupDTO | null | undefined>(undefined);
  const [form, setForm] = useState<EgressGroupInput>(empty);
  const [content, setContent] = useState("");
  const save = useMutation({ mutationFn: () => editing ? updateEgressGroup(editing.id, form) : createEgressGroup(form), onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-groups"] }); setEditing(undefined); toast.success(t("settings.egressGroups.saved")); } });
  const remove = useMutation({ mutationFn: deleteEgressGroup, onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-groups"] }); toast.success(t("settings.egressGroups.deleted")); } });
  const importing = useMutation({ mutationFn: () => editing ? importEgressGroup(editing.id, content) : Promise.reject(new Error("group required")), onSuccess: (value) => { setContent(""); void queryClient.invalidateQueries({ queryKey: ["egress-groups"] }); toast.success(t("settings.egressGroups.imported", { count: value.items.filter((item) => item.created || item.reused).length })); } });
  const openCreate = () => { setForm(empty); setEditing(null); };
  const openEdit = (value: EgressGroupDTO) => { setForm({ name: value.name, scope: value.scope, enabled: value.enabled, strategy: value.strategy, maxConcurrency: value.maxConcurrency, fallbackGroupId: value.fallbackGroupId }); setEditing(value); };
  const groups = query.data?.items ?? [];
  const fallbackGroupLabel = i18n.language.startsWith("zh") ? "备用代理组" : "Fallback group";
  const noFallbackLabel = i18n.language.startsWith("zh") ? "不使用备用组" : "No fallback";
  return <div className="space-y-3">
    <div className="flex items-center justify-between gap-3"><p className="text-xs text-muted-foreground">{t("settings.egressGroups.description")}</p><Button type="button" size="sm" variant="secondary" onClick={openCreate}><Plus />{t("settings.egressGroups.add")}</Button></div>
    <div className="overflow-hidden rounded-md border"><table className="w-full text-xs"><thead><tr className="border-b text-left"><th className="px-3 py-2">{t("settings.egressGroups.name")}</th><th>{t("settings.egressGroups.scope")}</th><th>{t("settings.egressGroups.strategy")}</th><th>{t("settings.egressGroups.members")}</th><th className="w-24" /></tr></thead><tbody>{groups.map((group) => <tr className="border-b last:border-0" key={group.id}><td className="px-3 py-2 font-medium">{group.name}</td><td>{group.scope}</td><td>{group.strategy}</td><td>{group.enabledMembers}/{group.memberCount}</td><td><div className="flex gap-1"><Button variant="ghost" size="icon" className="size-7" aria-label={t("common.edit")} onClick={() => openEdit(group)}><Pencil /></Button><Button variant="ghost" size="icon" className="size-7 text-destructive" aria-label={t("common.delete")} onClick={() => remove.mutate(group.id)}><Trash2 /></Button></div></td></tr>)}</tbody></table></div>
    <Dialog open={editing !== undefined} onOpenChange={(open) => { if (!open) setEditing(undefined); }}><DialogContent><DialogHeader><DialogTitle>{editing ? t("settings.egressGroups.editTitle") : t("settings.egressGroups.addTitle")}</DialogTitle></DialogHeader><div className="grid gap-3"><Input placeholder={t("settings.egressGroups.name")} value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /><Select value={form.scope} onValueChange={(value) => setForm({ ...form, scope: value as EgressScope, fallbackGroupId: undefined })}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{(["grok_build", "grok_web", "grok_console", "grok_web_asset"] as EgressScope[]).map((scope) => <SelectItem key={scope} value={scope}>{scope}</SelectItem>)}</SelectContent></Select><Select value={form.strategy} onValueChange={(value) => setForm({ ...form, strategy: value as EgressGroupStrategy })}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{(["least_load", "weighted", "sticky", "round_robin"] as EgressGroupStrategy[]).map((strategy) => <SelectItem key={strategy} value={strategy}>{strategy}</SelectItem>)}</SelectContent></Select><Select value={form.fallbackGroupId ?? "none"} onValueChange={(value) => setForm({ ...form, fallbackGroupId: value === "none" ? undefined : value })}><SelectTrigger><SelectValue placeholder={fallbackGroupLabel} /></SelectTrigger><SelectContent><SelectItem value="none">{noFallbackLabel}</SelectItem>{groups.filter((group) => group.id !== editing?.id && group.scope === form.scope).map((group) => <SelectItem key={group.id} value={group.id}>{group.name}</SelectItem>)}</SelectContent></Select><Input type="number" min={0} placeholder={t("settings.egressGroups.maxConcurrency")} value={form.maxConcurrency} onChange={(event) => setForm({ ...form, maxConcurrency: Number(event.target.value) || 0 })} /><div className="flex items-center gap-2 text-xs"><Switch checked={form.enabled} onCheckedChange={(enabled) => setForm({ ...form, enabled })} />{t("settings.egressGroups.enabled")}</div>{editing ? <><Textarea placeholder={t("settings.egressGroups.importPlaceholder")} value={content} onChange={(event) => setContent(event.target.value)} /><Button type="button" variant="secondary" disabled={!content.trim() || importing.isPending} onClick={() => importing.mutate()}><Upload />{t("settings.egressGroups.import")}</Button></> : null}</div><DialogFooter><Button variant="outline" onClick={() => setEditing(undefined)}>{t("common.cancel")}</Button><Button disabled={!form.name.trim() || save.isPending} onClick={() => save.mutate()}>{t("common.save")}</Button></DialogFooter></DialogContent></Dialog>
  </div>;
}
