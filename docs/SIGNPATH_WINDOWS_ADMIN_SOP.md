# Reasonix Windows SignPath 配置与验收 SOP

本文供 SignPath 管理员、GitHub 仓库管理员和 Release Maintainer 配置并验收
Reasonix Windows Authenticode 两阶段签名链路。

关联变更：

- PR：[esengine/DeepSeek-Reasonix#6904](https://github.com/esengine/DeepSeek-Reasonix/pull/6904)
- 本 SOP 的验收对象：每次执行前通过 PR API 读回的当前 PR Head
- 签名工作流：`.github/workflows/release-desktop.yml`
- Authenticode 验证脚本：`scripts/verify-windows-authenticode.ps1`

> 如果 PR Head 已经发生变化，必须重新审查新的 commit 和 workflow diff，
> 不得继续使用本文记录的旧 SHA 进行正式 Secrets 验证。

## 1. 目标与完成标准

本次配置需要完成两阶段 Windows 签名：

1. 使用 `windows-payload` 给安装后实际落盘的 6 个 EXE 签名。
2. 使用 `windows-installer-v2` 验证这 6 个 EXE 已经签名，再给最终 NSIS
   安装器签名。

完整放行标准：

- `windows-payload` 和 `windows-installer-v2` 均已导入 SignPath 且状态为
  `VALID`。
- 旧的 `windows-installer` 仍然存在并保持 `DEFAULT`，未被覆盖或删除。
- `release-signing` 使用正式证书、Trusted Build System 和 Origin
  Verification。
- `release-signing` 保留证书强制要求的 SignPath 审批，但由专用
  `CI builds` 账号在 GitHub `release` environment 获批后自动完成；发布人
  只需在 GitHub 批准一次。
- `test-signing-ci-approval` 使用测试证书和同一自动审批链路。
- AMD64 和 ARM64 canary 均完成两阶段签名。
- 正式证书 RC 中，所有 Authenticode 签名均为 `Status = Valid`。
- Windows Defender 环境下安装、启动、更新和卸载均通过。

## 2. 管理员分工

| 角色 | 责任 |
| --- | --- |
| SignPath 组织管理员 / 项目 Configurator | 导入 Artifact Configuration，维护签名策略和 CI User 权限 |
| GitHub 仓库管理员 | 维护 Actions Secrets/Variables，必要时建立官方临时验证分支 |
| Release Maintainer | 批准 GitHub `release` environment，发布并验收正式证书 RC |

如果现有维护者没有 SignPath 项目配置权限，组织管理员可以直接执行导入，
或者在项目设置中将维护者或维护者组添加到 `Configurators`。

SignPath 权限说明：
[Users and permissions](https://docs.signpath.io/users/)

## 3. 当前配置基线

截至 2026-07-24 的只读核对结果：

- SignPath 组织：`DeepSeek-Reasonix [OSS]`
- SignPath 项目：`DeepSeek-Reasonix`
- 项目状态：`VALID`
- Repository URL：
  `https://github.com/esengine/DeepSeek-Reasonix.git`
- 当前 Artifact Configurations：
  - `Initial version`
  - `windows-installer`，状态为 `DEFAULT`
  - `windows-installer-test-v2`
  - `windows-installer-v2`
  - `windows-payload`
- `release-signing` 已开启 Trusted Build System 和 Origin Verification。
- `release-signing` 开启 `Use approval process`，Required approvals 为 `1`。
- `release-signing` 的 Allowed build definitions 仅允许
  `.github/workflows/release-desktop.yml`。
- `release-signing` 的 Allowed branches 必须精确为 `main-v2`；稳定版和 RC
  标签由最小 relay workflow 转发到该受保护控制面。
- `test-signing-ci-approval` 使用测试证书，只允许 `CI builds` 提交和审批，
  Required approvals 为 `1`，并启用相同的 Trusted Build、Origin 和 Build
  Definition 限制。
- `Release certificate 2026` 在证书 Restrictions 中启用了
  `Requires approval process`，因此该审批不能在项目策略中关闭。

## 4. 授予 SignPath 配置权限

如果由现有组织管理员亲自导入，可以跳过本节。

1. 登录 SignPath。
2. 进入 `Projects`。
3. 打开 `DeepSeek-Reasonix`。
4. 进入项目编辑或项目权限设置。
5. 在 `Configurators` 中添加负责维护签名配置的用户或用户组。
6. 保存。
7. 重新打开项目。
8. 确认 `Artifact Configurations` 区域出现 `Add` 按钮。

建议授权给维护者组，而不是长期绑定单个账号。

## 5. 导入 `windows-payload`

### 5.1 获取固定版本 XML

必须从经过审查的 PR commit 复制文件，不要手工重新编写 XML：

- 仓库路径：
  `.signpath/artifact-configurations/windows-payload.xml`
- 固定版本：
  [windows-payload.xml@fe354e5](https://github.com/SivanCola/DeepSeek-Reasonix/blob/fe354e59a9a076930403b7d8aefb0bcd0b4e182a/.signpath/artifact-configurations/windows-payload.xml)

### 5.2 导入步骤

1. SignPath → `Projects` → `DeepSeek-Reasonix`。
2. 找到 `Artifact Configurations`。
3. 点击 `Add`。
4. 选择 `Custom`。
5. 名称填写：

   ```text
   windows-payload
   ```

6. Slug 必须为：

   ```text
   windows-payload
   ```

7. 粘贴上述文件的完整 XML。
8. 保存。
9. 确认配置状态为 `VALID`。
10. 点击 `Open XML`，逐字核对线上 XML 与仓库文件一致。
11. 不要将该配置设为 `DEFAULT`。

### 5.3 配置含义

GitHub `upload-artifact` 提交给 SignPath 的产物是 ZIP，因此配置根节点必须是
`<zip-file>`。

该配置需要给以下 6 个 EXE 执行 `authenticode-sign`：

- `reasonix-desktop.exe`
- `reasonix-guard.exe`
- `reasonix-launcher.exe`
- `reasonix-update-helper.exe`
- `reasonix-cli.exe`
- `reasonix-uninstall.exe`

参考：

- [Artifact configurations](https://docs.signpath.io/artifact-configuration/)
- [GitHub trusted build system](https://docs.signpath.io/trusted-build-systems/github)
- [Artifact configuration syntax](https://docs.signpath.io/artifact-configuration/syntax)

## 6. 导入 `windows-installer-v2`

### 6.1 获取固定版本 XML

- 仓库路径：
  `.signpath/artifact-configurations/windows-installer-v2.xml`
- 固定版本：
  [windows-installer-v2.xml@fe354e5](https://github.com/SivanCola/DeepSeek-Reasonix/blob/fe354e59a9a076930403b7d8aefb0bcd0b4e182a/.signpath/artifact-configurations/windows-installer-v2.xml)

### 6.2 导入步骤

1. 再次点击 `Artifact Configurations → Add → Custom`。
2. 名称填写：

   ```text
   windows-installer-v2
   ```

3. Slug 必须为：

   ```text
   windows-installer-v2
   ```

4. 粘贴上述文件的完整 XML。
5. 保存。
6. 确认配置状态为 `VALID`。
7. 点击 `Open XML`，逐字核对线上 XML 与仓库文件一致。
8. 不要将该配置设为 `DEFAULT`。

### 6.3 配置含义

该配置需要：

1. 对最终的 `*installer*.exe` 执行 `authenticode-sign`。
2. 对上述 6 个内层 EXE 执行 `authenticode-verify`。

如果任一内层 EXE 未签名，或签名后又被修改，第二阶段请求必须失败，不能继续
生成可发布的安装器。

参考：

- [Artifact configuration reference](https://docs.signpath.io/artifact-configuration/reference)
- [Projects and versioned configurations](https://docs.signpath.io/projects)

## 7. 保留旧配置

导入完成后的 Artifact Configurations 应为：

| 配置 | 预期状态 |
| --- | --- |
| `Initial version` | 保留 |
| `windows-installer` | 保留并继续为 `DEFAULT` |
| `windows-payload` | 新增、`VALID`、非 `DEFAULT` |
| `windows-installer-v2` | 新增、`VALID`、非 `DEFAULT` |

禁止执行以下操作：

- 删除 `windows-installer`。
- 将 `windows-installer` 的 XML 替换为新配置。
- 修改旧配置的 Slug。
- 将两个新配置设置为 DEFAULT。

新工作流通过明确的
`artifact-configuration-slug` 选择配置，不需要改变默认配置。保留旧配置是为了
保证旧 release ref 仍然可重跑。

## 8. 检查并修正签名策略

### 8.1 `test-signing`

打开 `test-signing` 并确认：

- 使用测试证书。
- Submitters 包含 `CI builds`。
- CI 请求可以自动完成，不要求人工审批。
- 如果启用 Origin Verification，仓库地址必须为：

  ```text
  https://github.com/esengine/DeepSeek-Reasonix.git
  ```

### 8.2 `release-signing`

打开 `release-signing → Edit` 并设置：

- Purpose：`Release signing`
- Certificate：正式 Release certificate
- Submitters：必须包含 `CI builds`
- `Require trusted build system`：开启
- `Verify origin policy`：开启
- Repository URL：

  ```text
  https://github.com/esengine/DeepSeek-Reasonix.git
  ```

- Allowed branches：**只能填写 `main-v2`**
- `Use approval process`：**保持开启**
- Required approvals：`1`
- Approvers：必须至少包含能够处理正式发布的 SignPath 人工审批人

保存后重新打开策略，确认：

- 策略状态为 `VALID`。
- `Use approval process` 已开启。
- Trusted Build System 和 Origin Verification 仍然开启。
- Allowed branches 精确显示 `main-v2`，没有 `**`、`v*`、
  `desktop-v*` 或临时测试分支。

正式版和 RC 的标签事件由 `release-stable-trigger.yml` /
`release-desktop-trigger.yml` 转发：relay 只携带候选 tag，实际
`release-desktop.yml` 固定运行在受保护的 `main-v2`，再签署该 tag 指向的
不可变候选 SHA。不能把 Allowed branches 改成标签通配符，因为普通分支也可以
取形如 `v-malicious` 的名字。

`Release certificate 2026` 的 Restrictions 明确要求所有使用该证书的策略启用
审批流程。尝试关闭时，SignPath 会拒绝保存并提示：

```text
Certificate requires an approval process.
You can either enable the approval process or use another certificate.
```

因此不得关闭正式签名策略的审批。工作流先以
`wait-for-completion: false` 提交请求，取得 Signing Request ID，再由专用
`CI builds` 账号调用 SignPath `Approve` API，并轮询、下载签名产物。Canary
使用 `test-signing-ci-approval`，以测试证书验证完全相同的自动审批流程。

## 9. 检查 GitHub Actions 配置

进入：

`Settings → Secrets and variables → Actions`

确认以下 Repository Secrets 存在：

- `SIGNPATH_API_TOKEN`
- `SIGNPATH_ORGANIZATION_ID`

安全要求：

- 不得在日志、截图、Issue、PR 评论或聊天中显示 Secret 值。
- Token 对应的 SignPath CI User 应为 `CI builds`。
- `CI builds` 必须同时具备 `test-signing-ci-approval` 和
  `release-signing` 的 Submitter、Approver 权限。
- 不得把个人 Interactive User 的 Token 用作 `SIGNPATH_API_TOKEN`。
- `SIGNPATH_ORGANIZATION_ID` 必须指向正确的 OSS 组织。
- GitHub `release` environment 的审批人仍然有效。
- `release-signing` 的 Allowed branches 精确为 `main-v2`。

正式验收前，应保持 release readiness 关闭：

```bash
gh variable set SIGNPATH_RELEASE_SIGNING_READY \
  --repo esengine/DeepSeek-Reasonix \
  --body false
```

这会使正式版和 RC 在 SignPath 未完成验收时失败关闭。

## 10. 运行 AMD64/ARM64 Canary

### 10.1 Fork PR 的限制

PR #6904 来自 fork。Fork 工作流拿不到官方仓库的 SignPath Secrets，因此不能
直接在 fork PR 上完成真实签名。

可以选择：

1. 合并后从 `main-v2` 运行 canary。
2. 如果必须合并前验证，由官方仓库管理员创建一个临时验证分支，并将它精确
   指向已经完成安全审查的 PR SHA；生产证书烟测期间临时把该分支加入 SignPath
   白名单，并关闭 `CI builds` 自动审批，烟测完成后立即恢复。

### 10.2 合并前临时验证分支

推送前先重新获取 PR Head：

```bash
PR_SHA="$(gh pr view 6904 \
  --repo esengine/DeepSeek-Reasonix \
  --json headRefOid \
  --jq .headRefOid)"

git fetch \
  https://github.com/SivanCola/DeepSeek-Reasonix.git \
  fix/windows-payload-authenticode

test "$(git rev-parse FETCH_HEAD)" = "$PR_SHA"

git push origin \
  "${PR_SHA}:refs/heads/test/windows-signpath-v2"
```

临时分支上的 workflow 会接触官方 Secrets。推送前必须重新检查该 SHA 的完整
workflow diff、恶意代码风险和 CI 状态，并把复核后的 `$PR_SHA` 记录到变更单；
不要在 SOP 中硬编码一个会过期的 SHA。

### 10.3 触发测试证书 Canary

```bash
gh workflow run release-desktop.yml \
  --repo esengine/DeepSeek-Reasonix \
  --ref test/windows-signpath-v2 \
  -f channel=canary \
  -f base_version=X.Y.Z
```

将 `X.Y.Z` 替换为计划中的下一版本号。

现有 canary 不是纯 dry-run：

- 会更新 R2 `canary/` 产物指针。
- 不会创建 GitHub Release。
- 不会移动稳定版 `latest/`。

运行前应确认更新 canary 产物已经得到允许。

### 10.4 触发正式证书、零发布烟测

PR 中的 `production_signing_smoke` 模式会经过 GitHub `release` environment
审批，使用 `release-signing` 与正式证书验证 AMD64/ARM64，但跳过 publish
job，不创建 Release，也不更新 R2 指针：

```bash
gh workflow run release-desktop.yml \
  --repo esengine/DeepSeek-Reasonix \
  --ref test/windows-signpath-v2 \
  -f channel=canary \
  -f base_version=X.Y.Z \
  -f production_signing_smoke=true
```

合并前执行时必须遵守以下临时窗口：

1. SignPath `release-signing` 暂时允许
   `main-v2` 和 `test/windows-signpath-v2`。
2. 暂时从 Approvers 移除 `CI builds`，只保留正式人工审批人。
3. GitHub `release` environment 获批后，工作流只轮询请求状态，不调用
   SignPath Approve API；人工核对并批准 4 个 SignPath 请求。
4. 确认两种架构的验证结果均为 `Status = Valid`。
5. 立即把 Allowed branches 恢复为仅 `main-v2`，再恢复 `CI builds`
   Approver。

不得让“临时分支可使用正式证书”和“CI 自动审批”同时生效。

### 10.5 监控运行

```bash
RUN_ID="$(gh run list \
  --repo esengine/DeepSeek-Reasonix \
  --workflow release-desktop.yml \
  --branch test/windows-signpath-v2 \
  --event workflow_dispatch \
  --limit 1 \
  --json databaseId \
  --jq '.[0].databaseId')"

gh run watch "$RUN_ID" \
  --repo esengine/DeepSeek-Reasonix \
  --exit-status
```

## 11. Canary 验收标准

以下两个任务必须同时成功：

- `build (windows-amd64)`
- `build (windows-arm64)`

每个平台必须完成：

1. 构建未签名 payload。
2. 上传 payload。
3. 使用 `windows-payload` 签署 6 个 EXE；`CI builds` 自动批准请求。
4. 使用已签 payload 重新生成 portable ZIP 和 NSIS 安装器。
5. 上传 installer signing bundle。
6. Canary 使用 `windows-installer-test-v2` 签署外层安装器；正式版使用
   `windows-installer-v2` 验证内层签名并签署外层安装器。两者均由
   `CI builds` 自动批准请求。
7. 执行 Authenticode release contract 验证。
8. 发布 canary 产物。

SignPath Signing Requests 中应出现 4 个成功请求：

| 架构 | 第一阶段 | 第二阶段 |
| --- | --- | --- |
| AMD64 | payload 签名 | installer 验证及签名 |
| ARM64 | payload 签名 | installer 验证及签名 |

逐个检查：

- 状态为 `Completed`。
- Artifact Configuration Slug 正确。
- Origin 指向官方仓库。
- Commit SHA 与 GitHub Actions 运行 SHA 一致。
- Trusted Build、Origin Verification、Malware Scan 均通过。
- Processing Log 中的批准 Actor 为 `CI builds`。
- 没有等待 SignPath 人工确认。

Canary 使用测试证书，因此本地 Windows 可能显示证书链不受信任。Canary 阶段
主要验证每个文件确实存在 Authenticode 签名以及两阶段产物关系正确。正式
信任链在 RC 阶段验证。

## 12. 开启正式签名门禁

只有以下条件全部满足后才能开启：

- 两个新 Artifact Configuration 均为 `VALID`。
- 旧 `windows-installer` 未改变。
- `release-signing` 的证书级审批保持开启，正式审批人可用。
- `release-signing` 和 `test-signing-ci-approval` 的 Build Definition 仅允许
  `.github/workflows/release-desktop.yml`。
- `release-signing` 的 Allowed branches 精确为 `main-v2`。
- `CI builds` 是两个策略的 Submitter 和 Approver，GitHub Secret 使用其专用
  Token。
- AMD64 和 ARM64 canary 全部成功。
- 4 个 SignPath Signing Request 全部成功。

设置：

```bash
gh variable set SIGNPATH_RELEASE_SIGNING_READY \
  --repo esengine/DeepSeek-Reasonix \
  --body true
```

读回确认：

```bash
gh variable get SIGNPATH_RELEASE_SIGNING_READY \
  --repo esengine/DeepSeek-Reasonix
```

## 13. 合并后运行正式证书 RC

测试证书 Canary 只证明签名链路可以工作。合并前已完成
`production_signing_smoke=true` 的双架构正式证书验证时，RC 用于验证真实
公开 prerelease 发布；未完成烟测时，PR 合并到 `main-v2` 后必须先运行一次
正式证书 RC。

首先核对目标 commit：

```bash
git fetch origin main-v2 --tags
RC_SHA="$(git rev-parse origin/main-v2)"
git show --no-patch --format='%H %s' "$RC_SHA"
```

在得到 Release Maintainer 明确授权后创建并推送 RC tag：

```bash
git tag "desktop-vX.Y.Z-rc.1" "$RC_SHA"
git push origin "desktop-vX.Y.Z-rc.1"
```

该操作会：

- 触发一次 GitHub `release` environment 审批。
- 使用 `release-signing` 和正式证书。
- 创建公开的 GitHub prerelease。
- 不移动 R2 稳定版 `latest/`。

RC tag 是外部发布动作，不得在未获得发布授权时执行。

## 14. 正式证书与 Defender 验收

RC 的 Windows 验证会启用 `RequireTrusted=true`。所有 Authenticode 签名必须
返回：

```text
Status = Valid
```

在干净的 Windows 11 AMD64 和 ARM64 环境中检查安装目录：

```powershell
Get-ChildItem "<Reasonix安装目录>" -Recurse -Filter *.exe |
  ForEach-Object {
    $signature = Get-AuthenticodeSignature $_.FullName
    [PSCustomObject]@{
      File    = $_.FullName
      Status  = $signature.Status
      Subject = $signature.SignerCertificate.Subject
    }
  }
```

验收要求：

- 安装器签名为 `Valid`。
- 安装目录内全部 6 个 EXE 签名为 `Valid`。
- Portable ZIP 内的可执行文件签名为 `Valid`。
- AMD64 和 ARM64 的签名证书 Subject 符合预期。
- Windows Defender 保持开启。
- 实际完成安装、首次启动、CLI 调用、更新和卸载。
- Windows Security → Protection History 中没有新隔离或拦截。

只有 AMD64 和 ARM64 都通过，才可以继续正式稳定版发布。

## 15. 失败处理与回滚

| 现象 | 处理 |
| --- | --- |
| 看不到 `Add` | 添加项目 Configurator，或由组织管理员直接导入 |
| XML 无法保存或状态不是 `VALID` | 只修复新配置，不修改旧 `windows-installer` |
| Canary Signing Request 长时间 Pending | `test-signing` 不应要求审批；检查 CI User、配置 Slug 和请求错误 |
| Release Signing Request 显示 Pending approval | 这是正式证书的强制门禁；由授权 SignPath 审批人在 Action 超时前处理 |
| Origin Verification 失败 | 核对仓库 URL、ref、SHA 和 GitHub Trusted Build |
| 提示文件缺失或存在额外文件 | 检查 signing bundle 与 XML 文件清单是否一致 |
| `authenticode-verify` 失败 | 检查内层文件是否未签名，或签名后被重新编译/修改 |
| 只有 AMD64 成功 | 不放行，ARM64 也是硬门槛 |
| RC 签名不是 `Status = Valid` | 关闭 readiness，禁止稳定发布 |
| 正式请求等待人工审批 | 关闭 readiness，检查 `CI builds` Approver 权限、API Token 和自动审批步骤；不得关闭证书强制审批 |

发生正式签名故障时，立即恢复失败关闭：

```bash
gh variable set SIGNPATH_RELEASE_SIGNING_READY \
  --repo esengine/DeepSeek-Reasonix \
  --body false
```

在故障解除并重新完成 AMD64/ARM64 验收前，不得发布稳定版。

## 16. 最终签字清单

- [ ] `windows-payload` 已导入且为 `VALID`
- [ ] `windows-installer-v2` 已导入且为 `VALID`
- [ ] 旧 `windows-installer` 仍存在并保持 `DEFAULT`
- [ ] `test-signing-ci-approval` 已创建且为 `VALID`
- [ ] `CI builds` 是 `release-signing` 和 `test-signing-ci-approval` 的 Submitter、Approver
- [ ] `SIGNPATH_API_TOKEN` 对应专用 `CI builds`，不是个人账号
- [ ] `release-signing` 已开启 Trusted Build System
- [ ] `release-signing` 已开启 Origin Verification
- [ ] 两个自动审批策略的 Allowed build definitions 均为 `.github/workflows/release-desktop.yml`
- [ ] `release-signing` 的 Allowed branches 精确为 `main-v2`
- [ ] `release-signing` 的 SignPath 审批已开启，Required approvals 为 `1`
- [ ] GitHub `release` environment 的正式发布审批人和响应流程已经明确
- [ ] AMD64 canary 两阶段签名成功
- [ ] ARM64 canary 两阶段签名成功
- [ ] 4 个 SignPath Signing Request 均为 `Completed`
- [ ] `SIGNPATH_RELEASE_SIGNING_READY=true`
- [ ] 正式证书 RC 的 AMD64 签名均为 `Valid`
- [ ] 正式证书 RC 的 ARM64 签名均为 `Valid`
- [ ] Defender 安装、启动、更新和卸载验证通过
- [ ] 签名稳定版已真实可下载后，再关闭对应问题单

## 17. 参考资料

- [SignPath Artifact Configuration](https://docs.signpath.io/artifact-configuration/)
- [SignPath Artifact Configuration Syntax](https://docs.signpath.io/artifact-configuration/syntax)
- [SignPath Artifact Configuration Reference](https://docs.signpath.io/artifact-configuration/reference)
- [SignPath Projects](https://docs.signpath.io/projects)
- [SignPath Users and Permissions](https://docs.signpath.io/users/)
- [SignPath GitHub Trusted Build System](https://docs.signpath.io/trusted-build-systems/github)
- [Reasonix PR #6904](https://github.com/esengine/DeepSeek-Reasonix/pull/6904)
