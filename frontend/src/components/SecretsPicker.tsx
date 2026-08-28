import { useEffect, useState } from "react";
import { Label } from "@/components/ui/label";

type SecretMeta = { id: string; name: string; description: string };

export function SecretsPicker({ value, onChange }: { value: string[]; onChange: (ids: string[]) => void }) {
  const [secrets, setSecrets] = useState<SecretMeta[]>([]);
  const [loading, setLoading] = useState(false);
  useEffect(() => {
    let cancelled = false;
    (async () => {
      setLoading(true);
      try {
        const res = await fetch("/api/orchicon.api.v1.SecretsService/ListSecrets", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({}) });
        const data = await res.json();
        if (!cancelled) setSecrets((data.secrets || []).map((s: any) => ({ id: s.id, name: s.name, description: s.description })));
      } catch { /* ignore */ } finally { if (!cancelled) setLoading(false); }
    })();
    return () => { cancelled = true; };
  }, []);
  const toggle = (id: string) => {
    if (value.includes(id)) onChange(value.filter((v) => v !== id));
    else {
      if (value.length >= 10) return;
      onChange([...value, id]);
    }
  };
  if (loading) return <p className="text-xs text-muted-foreground">Loading secrets…</p>;
  if (secrets.length === 0) return <p className="text-xs text-muted-foreground">No secrets yet — create them in Settings → Secrets.</p>;
  return (
    <div className="space-y-2">
      <Label>Secrets to inject (max 10)</Label>
      <p className="text-xs text-muted-foreground">Selected secrets are decrypted plane-side and injected as container env (-e) at dispatch. Never baked into images.</p>
      <div className="rounded-xl border border-border/60 p-2 space-y-1 max-h-48 overflow-auto">
        {secrets.map((s) => (
          <label key={s.id} className="flex items-center gap-2 rounded px-2 py-1 hover:bg-accent cursor-pointer">
            <input type="checkbox" checked={value.includes(s.id)} onChange={() => toggle(s.id)} className="h-4 w-4 rounded border-input" />
            <span className="font-mono text-xs">{s.name}</span>
            <span className="text-xs text-muted-foreground truncate">{s.description}</span>
          </label>
        ))}
      </div>
      {value.length > 0 && <p className="text-xs text-muted-foreground">{value.length} selected</p>}
    </div>
  );
}
