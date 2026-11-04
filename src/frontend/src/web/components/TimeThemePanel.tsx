import type { CSSProperties } from 'react';
import { formatHour, themeTrack, type TimeThemeControls } from '../timeTheme';
import type { Translate } from '../i18n';

interface Props {
  timeTheme: TimeThemeControls;
  t: Translate;
}

// 门户自己的外观设置：跟随本地时间 + 主题时段滑块。
// 它属于“门户”这个服务页，而不是所有服务共用的页头。
export function TimeThemePanel({ timeTheme, t }: Props) {
  return (
    <section className="appearance" aria-label={t('ui.settings.appearance', '外观')}>
      <h2 className="set-label">{t('ui.settings.appearance', '外观')}</h2>
      <div className="form">
        <div className="field">
          <div className="field-head">
            <span className="field-label">{t('ui.settings.followTime', '跟随时间')}</span>
            <span className="field-badges">
              <span className={timeTheme.auto ? 'tag-default' : 'tag-over'}>
                {timeTheme.auto
                  ? t('ui.settings.following', '跟随中')
                  : t('ui.settings.fixed', '已固定')}
              </span>
            </span>
          </div>
          <div className="field-control">
            <button
              type="button"
              className="switch"
              role="switch"
              aria-checked={timeTheme.auto}
              aria-label={t('ui.settings.followTime', '跟随时间')}
              onClick={() => {
                timeTheme.setAuto(!timeTheme.auto);
              }}
            >
              <span className="switch-knob" />
            </button>
          </div>
          <p className="field-hint">
            {t(
              'ui.settings.followTimeHint',
              '每分钟按本地时间重新取色；整体背景、强调色与深/浅面板都会自动跟随。',
            )}
          </p>
        </div>

        <div className="field">
          <div className="field-head">
            <span className="field-label">{t('ui.settings.themeTime', '主题时段')}</span>
            <span className="field-badges">
              <span className="tag-over">{timeTheme.label}</span>
            </span>
          </div>
          <div className="field-control">
            <input
              type="range"
              className="range"
              min={0}
              max={24}
              step={0.25}
              value={timeTheme.hour}
              aria-label={t('ui.settings.themeTime', '主题时段')}
              aria-valuetext={`${formatHour(timeTheme.hour)} · ${timeTheme.label}`}
              style={{ '--range-track': themeTrack() } as CSSProperties}
              onChange={(e) => {
                timeTheme.setHour(Number(e.target.value));
              }}
            />
            <span className="field-readout">{formatHour(timeTheme.hour)}</span>
          </div>
          <p className="field-hint">
            {timeTheme.auto
              ? `${t('ui.settings.followingNow', '跟随时间中 · 当前')}「${timeTheme.label}」；${t('ui.settings.dragToFix', '拖动滑块会固定到所选时段')}。`
              : `${t('ui.settings.fixedAt', '已固定为')}「${timeTheme.label}」；${t('ui.settings.reopenAuto', '打开「跟随时间」恢复自动')}。`}
          </p>
        </div>
      </div>
    </section>
  );
}
