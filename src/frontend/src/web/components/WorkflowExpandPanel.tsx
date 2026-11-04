import { WorkflowExpandOptions, type WorkflowExpandControls } from '../workflowExpand';
import type { Translate } from '../i18n';

interface Props {
  workflowExpand: WorkflowExpandControls;
  t: Translate;
}

// 门户自己的阅读偏好：工作流（深度思考 / 终端输出）什么时候自动展开。
// 和 TimeThemePanel 一样属于"门户"这个服务页，是本地偏好，不走服务设置协议。
export function WorkflowExpandPanel({ workflowExpand, t }: Props) {
  return (
    <section className="appearance" aria-label={t('ui.settings.workflow', '工作流')}>
      <h2 className="set-label">{t('ui.settings.workflow', '工作流')}</h2>
      <div className="form">
        <div className="field">
          <div className="field-head">
            <span className="field-label">{t('ui.settings.workflowExpand', '工作流展开')}</span>
            <span className="field-badges">
              <span className="tag-over">
                {t(`ui.settings.workflowExpand.${workflowExpand.mode}`, workflowExpand.mode)}
              </span>
            </span>
          </div>
          <div className="field-control">
            <div className="seg" role="radiogroup" aria-label={t('ui.settings.workflowExpand', '工作流展开')}>
              {WorkflowExpandOptions.map((mode) => (
                <button
                  key={mode}
                  type="button"
                  className="seg-item"
                  role="radio"
                  aria-checked={workflowExpand.mode === mode}
                  title={t(`ui.settings.workflowExpand.${mode}.hint`, '')}
                  onClick={() => {
                    workflowExpand.setMode(mode);
                  }}
                >
                  {t(`ui.settings.workflowExpand.${mode}`, mode)}
                </button>
              ))}
            </div>
          </div>
          <p className="field-hint">
            {t(
              'ui.settings.workflowExpandHint',
              '决定对话里的过程（深度思考、终端输出）什么时候自己摊开；不管选哪档，都能手动点开。',
            )}
          </p>
        </div>
      </div>
    </section>
  );
}
