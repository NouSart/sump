# automation

把「敲几条命令才能出的东西」收成一条命令。这里放的是**本机可执行的构建与打包脚本**，与 `.github/workflows/` 里托管的 CI 分开：CI 可以调这里的脚本，这里不依赖 CI。

## package.ps1

```powershell
.\automation\package.ps1                  # 全量打包 → dist\sump-<版本>-win-x64.zip
.\automation\package.ps1 -SkipFrontend    # 前端没改动时复用现有 dist，省一次 build
.\automation\package.ps1 -NoZip           # 只留目录，方便先检查内容
.\automation\package.ps1 -IncludeNodeModules   # 连前端运行时依赖一起打（体积 +100MB 左右）
```

它做五件事：

1. 用 `go build -trimpath` 编 11 个服务到 `bin\`（二进制名沿用 `run-local.ps1` 的约定）
2. `npm run build` 构建门户，收集 `dist\` 与两个 `package.json`
3. 收集 embedding 源码（**不含** `.venv`、`.pytest_cache` 与模型文件）
4. 写 `VERSION`、`MANIFEST.txt`、`SHA256SUMS.txt`
5. 核对产物：11 个可执行文件都在且体积正常、前端两个入口存在、`pyproject.toml` 存在——少一个就抛错退出

产物落在 `dist\`（已 gitignore）。版本号默认从 `README.md` 的 `**vX.Y.Z**` 行取，可用 `-Version` 覆盖。

## 这个包不是什么

- **不是一键安装包**：不含 Go / Node / Python 运行时，也不含模型文件与 `data\`。目标机要自己装 Node ≥ 22 与 Python ≥ 3.12，门户还要先 `npm ci --omit=dev`。
- **不含启动脚本**：`scripts\` 里的脚本按仓库布局写路径，放进包里会误导。要一键起全栈，仍然在仓库里跑 `.\scripts\run-local.ps1`。
- **只出 win-x64**：交叉编译（linux / darwin）还没做，见 `TODO.md`。

## 还没做的

- 版本号变更与 `CHANGELOG` 生成（发版流程）
- GitHub Release 上传
- 交叉编译与单文件安装器
