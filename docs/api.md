# HTTP 协议

所有业务路径以 `/v1` 开头。请求体使用 JSON，素材上传直接使用 PNG 字节。

| 方法和路径 | 请求 | 结果 |
|---|---|---|
| PUT /scenes/{id} | name, expectedRevision, document | 完整场景，200 |
| GET /scenes/{id} | 无 | 完整场景，200 |
| POST /scenes/{id}/state | expectedRevision, state | 完整场景，200 |
| POST /scenes/{id}/snapshots | expectedRevision | 快照和素材闭包，201 |
| PUT /assets/{id} | PNG字节 | 素材及base64字节，201 |
| POST /assets/{id}/retire | 无 | state，200 |
| GET /snapshots/{id} | 无 | 快照，200 |
| POST /snapshots/{id}/revoke | 无 | state，200 |
| GET /snapshots/{id}/package | 无 | atlas-scene交换包，200 |
| POST /imports/{id} | name, package | 无冲突时新场景201；冲突时409并附preview |
| POST /imports/{id}/preview | name, package | 只读冲突预览，200，不改动目标 |
| POST /imports/{id}/confirm | name, fingerprint, package, decisions | 按决策提交，201；过期指纹409 |
| POST /jobs | snapshotId, scale；请求头Idempotency-Key | 任务，202 |
| GET /jobs/{id} | 无 | 任务，200 |
| POST /jobs/{id}/cancel | 无 | 任务，200 |
| POST /jobs/{id}/retry | 无 | 任务，202 |
| GET /artifacts/{id} | 可选If-None-Match | PNG，200或304 |
| DELETE /artifacts/{id} | 无 | state，200 |
| POST /compare | left, right（结果ID） | 像素比较，200 |
| GET /status | 无 | 场景/素材/快照/任务计数和结果字节数 |
| GET /events | 无 | 最多500个有序领域事件 |

`GET /healthz` 不带 `/v1`，返回ready。它表明进程已打开并恢复状态，不包含单独的磁盘可写探测。任务响应省略固定输入的有效载荷，输入细节通过快照接口读取。

文档字段：width、height、background、layers。每个图层有id、kind、width、height、transform、opacity、fill；transform包括x、y、scaleX、scaleY、degrees。图层可以有clip（x/y/width/height），group使用children，polygon使用points，image使用assetId。solid颜料有color；linear还包括endColor、from、to。示例是可直接提交的完整请求。

幂等键长度1–80，使用英文字母、数字、下划线和连字符，首位不能是后两者。它在服务实例持久状态中全局生效，不随任务完成、取消、结果清理或进程重启失效。改换请求语义需使用新键。

服务接受相同内容的素材重复上传，退役素材不接受重新上传。交换包导出保留快照状态；导入会创建全新的active场景，原快照不作为目标服务中的快照记录落地。共享素材必须内容相同且仍可用。

## 交换包冲突预览与确认

`POST /imports/{id}/preview` 只做校验和只读分析，返回每个包内素材的 `status`：`new`（目标不存在，直接新增）、`identical`（同摘要且可用，直接复用）、`contentConflict`（同ID不同字节，可选 `keep` 或 `adopt`）、`retiredConflict`（目标已退役，只能 `adopt`）。预览同时给出目标与包内素材摘要、尺寸、允许决策和计数汇总，以及锁定当前目标事实的 `fingerprint`；目标场景已存在时 `sceneStatus` 为 `conflict`。

`POST /imports/{id}/confirm` 必须回传同一 `package`、`fingerprint` 和每个冲突素材的 `decisions`（键为包内素材ID，值为 `keep` 或 `adopt`）。所有判断在同一个持久事务内基于最新状态重算：指纹不一致返回409且不落地，客户端应重新拉取预览；缺少决策或决策不被允许返回400。`keep` 保留目标素材，导入文档引用原素材ID；`adopt` 把包内字节以全新素材ID落地并重写导入文档的引用，目标素材和任何已冻结快照/历史渲染的ID与字节都不变。退役素材只能 `adopt`，不会被复用或改名。

为兼容旧客户端，`POST /imports/{id}` 保留一次性语义：无冲突时直接201提交；存在冲突时返回409并在响应体 `preview` 字段附带与 preview 接口相同的预览，客户端再走 confirm 完成。任何确认失败都保持目标场景与素材原样，不会部分落地。
