// 门户自己的消息偏好：新消息到达且消息区已收起时，气泡只短暂展示多少秒。
// 与 TimeThemePanel / WorkflowExpandPanel 同一路数——纯本地偏好，不走服务设置协议。
import { BubbleSecondsOptions, type BubbleControls } from '../bubblePrefs';
import type { Translate } from '../i18n';

interface Props {
  bubble: BubbleControls;
  t: Translate;
}

export function BubblePanel({ bubble, t }: Props): React.ReactElement {
  return (
    <section className="appearance" aria-label={t('ui.settings.bubble', '消息气泡')}>
      <h2 className="set-label">{t('ui.settings.bubble', '消息气泡')}</h2>
      <div className="form">
        <div className="field">
          <div className="field-head">
            <span className="field-label">{t('ui.settings.bubblePeek', '新消息展示时长')}</span>
            <span className="field-badges">
              <span className="tag-over">{bubble.seconds}s</span>
            </span>
          </div>
          <div className="field-control">
            <div className="seg" role="radiogroup" aria-label={t('ui.settings.bubblePeek', '新消息展示时长')}>
              {BubbleSecondsOptions.map((seconds) => (
                <button
                  key={seconds}
                  type="button"
                  className="seg-item"
                  role="radio"
                  aria-checked={bubble.seconds === seconds}
                  onClick={() => {
                    bubble.setSeconds(seconds);
                  }}
                >
                  {seconds}s
                </button>
              ))}
            </div>
          </div>
          <p className="field-hint">
            {t(
              'ui.settings.bubblePeekHint',
              '消息区收起时，新消息只滑出气泡展示这么久，然后收回去；手动收起后不会被强行撑开。',
            )}
          </p>
        </div>
      </div>
    </section>
  );
}
