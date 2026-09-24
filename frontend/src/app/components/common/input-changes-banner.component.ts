import { ChangeDetectionStrategy, Component, computed, input } from '@angular/core';
import { LucideAngularModule } from 'lucide-angular';
import { InputChange } from '../../../types/scenario';

@Component({
  selector: 'app-input-changes-banner',
  standalone: true,
  imports: [LucideAngularModule],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (changes().length > 0) {
      <div class="stale-warning" [class.compact]="variant() === 'compact'">
        <header>
          <lucide-icon name="alert-triangle" [size]="compact() ? 14 : 16" />
          <strong>{{ compact() ? 'Inputs changed' : 'Inputs changed since this result was evaluated' }}</strong>
          <span class="count">{{ changes().length }}</span>
        </header>
        @if (variant() !== 'compact') {
          <ul>
            @for (change of visibleChanges(); track change.entity_type + change.entity_id + change.change_type) {
              <li><span class="tag" [attr.data-kind]="change.entity_type">{{ entityLabel(change.entity_type) }} · {{ change.change_type }}</span>{{ change.description }}</li>
            }
          </ul>
          @if (hiddenCount() > 0) {<small>...and {{ hiddenCount() }} more change(s)</small>}
          <p class="hint">{{ hint() }}</p>
        }
      </div>
    }
  `,
  styles: [`
    .stale-warning{border:1px solid #e7b4a8;border-left:3px solid #cf3f2e;background:#fdf3f1;color:#7a2418;padding:10px 12px}
    .stale-warning header{display:flex;align-items:center;gap:7px}.stale-warning header lucide-icon{color:#cf3f2e}.stale-warning strong{font-size:12px}.count{margin-left:auto;background:#cf3f2e;color:#fff;border-radius:9px;font-size:9px;font-weight:700;min-width:16px;height:16px;display:inline-grid;place-items:center;padding:0 4px}
    ul{margin:8px 0 0;padding:0;list-style:none;display:grid;gap:5px}li{font-size:11px;line-height:1.4;color:#4f302a;display:flex;gap:7px;align-items:baseline;flex-wrap:wrap}.tag{font-size:8px;text-transform:uppercase;letter-spacing:.06em;font-weight:700;padding:2px 6px;border-radius:3px;background:#f3d9d3;color:#94291a;white-space:nowrap}
    .hint{margin:8px 0 0;font-size:10px;color:#946a61}
    small{display:block;margin-top:6px;font-size:10px;color:#946a61}
    .stale-warning.compact{padding:7px 9px}.stale-warning.compact header{gap:6px}.stale-warning.compact strong{font-size:10px;text-transform:uppercase;letter-spacing:.04em}
  `]
})
export class InputChangesBannerComponent {
  readonly changes = input<InputChange[]>([]);
  readonly variant = input<'banner' | 'compact'>('banner');
  readonly maxShown = input(6);
  readonly hint = input('Ask the planner to re-evaluate before approving this result.');

  protected readonly compact = computed(() => this.variant() === 'compact');
  protected readonly visibleChanges = computed(() => this.changes().slice(0, this.maxShown()));
  protected readonly hiddenCount = computed(() => Math.max(0, this.changes().length - this.maxShown()));

  entityLabel(entityType: string): string {
    switch (entityType) {
      case 'thermal_zone': return 'Zone';
      case 'rack': return 'Rack';
      case 'equipment_load': return 'Load';
      case 'algorithm': return 'Algorithm';
      default: return entityType;
    }
  }
}
