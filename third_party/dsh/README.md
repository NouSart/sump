# DSH 设计系统副本（第三方，MIT）

**只用它的设计**，所以这里只放门户真正消费的那一块：上游 `ui-theme` 的样式表与品牌字体。

## 来源

| 项 | 值 |
| --- | --- |
| 仓库 | https://github.com/deepseek-ai/deepseek-harness |
| 提交 | `86a40220`（release `dsh-0.2.1-alpha.4`） |
| 许可 | MIT（见 `LICENSE`）；Montserrat 字体为 SIL OFL 1.1（见 `Montserrat-OFL.txt`） |
| 复制日期 | 2026-10-16 |

## 这里有什么

| 路径 | 内容 | 谁在用 |
| --- | --- | --- |
| `client/ui-theme/src/styles/design-platform.css` | 明/暗两套令牌：`--dsw-static-*` 原色 + `--dsw-alias-*` 语义色 | 本项目**所有颜色**都取自它（`frontend/src/web/styles.css` 直接用 `var(--dsw-alias-*)`，不自建色值） |
| `client/ui-theme/src/styles/base.css` | 字体栈、缓动、圆角阶梯 | 同上（字体、圆角、动效） |
| `client/ui-theme/src/styles/{corner-shape,focus,scrollbar}.css` | 圆角裁切、焦点环、滚动条 | 同上，整表 `@import` |
| `client/ui-theme/src/styles/brand-font.css` + `montserrat-{light,medium,regular}.woff2` | 品牌字 Montserrat | 侧栏品牌字 |
| `LICENSE`、`THIRD_PARTY_NOTICES.md` | 上游许可与第三方声明 | MIT 要求随副本保留 |

**这些文件逐字节原样保留**——观感差异在我们自己的 `styles.css` 里覆盖，不改上游文件。

上游 `ui-theme` 包**不能整块搬**：它直接 `import` cordis。这里只取 `src/styles/` 下的纯 CSS 与字体，零框架耦合。

其余 DSH 前端源码（`ui-primitives` 等，182 文件 / 1.0 MB）在分支 `feat/dsh-frontend-vendor` 的副本里，尚未合并。
