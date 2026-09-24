import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { LucideAngularModule } from 'lucide-angular';
import { InputChange, InputFreshness } from '../../../types/scenario';

@Component({
  selector: 'app-input-freshness-banner',
  standalone: true,
  imports: [LucideAngularModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (freshness(); as fresh) {
      @if (fresh.evaluated && fresh.stale) {
        <div class="stale-banner" role="alert">
          <div class="banner-heading">
            <span class="stale-tag"><lucide-icon name="alert-triangle" [size]="14" /> Inputs changed</span>
            <strong>{{ fresh.change_count }} changed input{{ fresh.change_count === 1 ? '' : 's' }} since this layout was evaluated</strong>
          </div>
          <p class="banner-message">{{ fresh.warning_message }}</p>
          <ul class="change-list">
            @for (change of grouped(); track change.key) {
              <li>
                <span class="entity" [class]="'kind ' + change.entity_type">{{ label(change.entity_type) }} {{ change.code }}</span>
                <span class="change-type">{{ change.change_type }}</span>
                <ul class="fields">
                  @for (field of change.fields; track field.name) {
                    <li>
                      <code>{{ field.name }}</code>
                      @if (field.old || field.next) {<span class="values"><del>{{ field.old || '—' }}</del><lucide-icon name="arrow-right" [size]="11" /><ins>{{ field.next || '—' }}</ins></span>}
                    </li>
                  }
                </ul>
              </li>
            }
          </ul>
          <p class="re-eval-hint"><lucide-icon name="rotate-ccw" [size]="12" /> Send the scenario back to draft and re-evaluate before approval.</p>
        </div>
      } @else if (fresh.evaluated) {
        <div class="fresh-banner"><lucide-icon name="check-circle-2" [size]="13" /><span>Inputs unchanged since evaluation — layout matches current racks, zones and loads.</span></div>
      }
    }
  `,
  styles: [`
    .stale-banner{border:1px solid #e1a29b;border-left:3px solid #cf3f2e;background:#fff5f3;border-radius:3px;padding:11px 13px;color:#7a2018}
    .banner-heading{display:flex;align-items:center;gap:9px;flex-wrap:wrap}.banner-heading strong{font-size:12px;color:#a1281e}
    .stale-tag{display:inline-flex;align-items:center;gap:5px;padding:3px 8px;border-radius:3px;background:#cf3f2e;color:#fff;font-size:9px;font-weight:800;text-transform:uppercase;letter-spacing:.04em;white-space:nowrap}
    .banner-message{margin:7px 0 8px;font-size:11px;color:#8a3a31}
    .change-list{list-style:none;margin:0;padding:0;display:grid;gap:7px}
    .change-list>li{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:2px 10px;align-items:start;background:#fff;border:1px solid #f0d4d0;border-radius:3px;padding:7px 9px}
    .entity{font-size:11px;font-weight:700;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.entity.kind{padding:2px 6px;border-radius:3px;margin-right:6px;font-size:9px;text-transform:uppercase;letter-spacing:.03em}
    .entity.thermal_zone{background:#fdeedc;color:#9a5a06}.entity.rack{background:#e7f0fb;color:#245d83}.entity.equipment_load{background:#efe9f9;color:#5b3a96}
    .change-type{font-size:9px;text-transform:uppercase;color:#a1281e;font-weight:700}
    .fields{grid-column:1/-1;list-style:none;margin:5px 0 0;padding:0;display:grid;gap:3px}.fields li{display:flex;align-items:baseline;gap:7px;flex-wrap:wrap;font-size:10px;color:#5a4540}
    .fields code{background:#f6e7e5;padding:1px 5px;border-radius:2px;color:#7a2018}.values{display:inline-flex;align-items:center;gap:5px;color:#68717a}.values del{text-decoration:line-through;opacity:.75}.values ins{text-decoration:none;font-weight:700;color:#a1281e}
    .re-eval-hint{display:flex;align-items:center;gap:6px;margin:9px 0 0;font-size:10px;font-weight:700;color:#a1281e}
    .fresh-banner{display:flex;align-items:center;gap:7px;padding:8px 12px;border:1px solid #9bcbb0;border-left:3px solid #237a4b;background:#eaf7ef;border-radius:3px;color:#237a4b;font-size:11px}
  `]
})
export class InputFreshnessBannerComponent {
  readonly freshness = input<InputFreshness | null | undefined>(null);

  readonly grouped = computed(() => {
    const changes = this.freshness()?.changes ?? [];
    const map = new Map<string, { key: string; entity_type: InputChange['entity_type']; code: string; change_type: string; fields: { name: string; old: string; next: string }[] }>();
    for (const change of changes) {
      const key = `${change.entity_type}:${change.entity_id}`;
      const existing = map.get(key);
      if (existing) {
        existing.fields.push({ name: change.field || change.change_type, old: change.old_value, next: change.new_value });
      } else {
        map.set(key, { key, entity_type: change.entity_type, code: change.code, change_type: change.change_type, fields: [{ name: change.field || change.change_type, old: change.old_value, next: change.new_value }] });
      }
    }
    return [...map.values()];
  });

  label(entityType: InputChange['entity_type']): string {
    switch (entityType) {
      case 'thermal_zone': return 'Zone';
      case 'rack': return 'Rack';
      case 'equipment_load': return 'Load';
    }
  }
}
