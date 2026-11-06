## 先更正我上一則的一句話

我說「少一幀 beat 一定抓得到」——**這句話不對**，我當時沒把 `onBeat` 的記帳讀乾淨。`applied` 被系統性地灌水，少一幀是偵測不到的。細節放在最後一節，因為它正好影響你問的生命週期。

---

## 一、「entry 幀」是什麼

socket 上一個 JSON text frame = 一個 event。`type: "entry"` 那一記就是 entry 幀，它的意義只有一句話：**這條已經寫進 log（path）了**。

```jsonc
{ "seq": 4212, "type": "entry", "session": "r2", "ts": "2026-07-31T13:04:12Z",
  "entry": { "id": "0000085", "parentId": "0000084", "kind": "assistant",
             "role": "assistant", "content": "…", "usage": {...}, "label": "llm error", ... },
  "streamId": "t1-1" }        // ← 只有「曾經串流出來」的 assistant 會帶
```

四種 `entry.kind`（`ir.go:94-97`）：`user` / `assistant` / `tool` / `compaction`。它跟另外三個容易混的東西部無關：`delta` 是**還沒落地**的串流片段、`tool_call`/`tool_result` 是**進度**（不是 log 裡的一條）、`state` 是**狀態列**。

順序上 daemon 一定先落地再發幀：`s.appendEntry(...)` 成功 → `s.emit(Event{Type: EvtEntry, Entry: committed, StreamID: streamID})`（`loop.go:526`）。`streamId` 只有在「這條是串流出來的 assistant」時才非空（`agent.go:52-59` 那段註解就是 D97 的出處），所以 `entry` 帶不帶 `streamId`，本身就等於「有沒有 buffer 要收」。

## 二、event 全表（`agent/agent.go:28-44`）

| type | 載體欄位 | 誰發（emit 點） | UI 做什麼 |
|---|---|---|---|
| `hello` | `info:"connected"` | `ws.go:166`（**不經 publish ⇒ `seq:0`、無 session**） | 標 conn 活著 |
| `entry` | `entry` (+`streamId`) | `loop.go:526`(assistant) / `969`(tool) / `session.go:581`(user) / `666`(compaction) / `park.go:514`(held) | 渲染一條正式訊息 / 認領 buffer |
| `delta` | `delta{streamId,content,reasoning,toolCall,done}` | `loop.go:738` / `641`(done) | 開 buffer、逐字、插 tool chip、收流 |
| `tool_call` | `run{id,callId,server,tool,arguments,status:"running"}` | `loop.go:870` | tool card 轉圈圈 |
| `tool_result` | `run{status:ok\|error\|parked,result,structuredContent,ms,entryId}` | `loop.go:912`(parked) / `968` | 結算卡片；`parked` 標成「掛起等待」 |
| `state` | `state`(整個 `Status`：phase/tip/entries/contextUsed/nCtx/servers/…) | `session.go:539` 各處 | 狀態列、busy、sidebar 計數 |
| `pending` | `pending: [ids]` | `session.go:543` | 排隊中的訊息清單 |
| `compaction` | `data{stage,attempt,…}` | `compact.go:775/469`、`loop.go:388/469/540/1109` | compacting 進度 |
| `request` | `gate{id,summarySize,…}` 或 `elicitation{...}` | `compact.go:873`（閘門）/ `session.go:1164`（追問） | 要使用者回答的卡片 |
| `request_done` | `info: <id>` | `compact.go:888`、`session.go:1168`(defer) | 撤掉卡片 |
| `log` | `level` + `info` | `registry.go:241/249/305/328/582`（MCP server 的嘮叨、tool 改名警告） | 只進 inspector／console |
| `error` | `info` | `loop.go:256`(panic)/370/705(llm)/815(stream)/956、`park.go:511` | toast；「loop is already running」被降級成 console |
| `info` | `info` | `loop.go:382/397/573/697`、`park.go:298/545` | toast |
| `removed` | `info: <entry id>` | `session.go:646`（rollback/edit/delete） | 拆節點＋日誌行 |
| `tools` | `data`(整個 `ToolsView`) | `session.go:1127` | 重開 tools drawer |

## 三、不是 event 的 frame（傳輸層自己）

- **`heartbeat`**：`{"type":"heartbeat","seq":<mgr.Seq()>}`，每 20s（`ws.go:49-65,122`）。它是**資料幀不是 ping 控制幀**，因為 JS 只看得见資料幀；`seq` 是 daemon 那把全域計數器，beat 就靠它。
- **`reply` / `reply_error`**：對 client 指令的回執（`ws.go:197-215`）。
- **client → server 只有三個指令**：`ping` / `subscribe` / `snapshot`。其他一律被拒：「this socket carries events, ping, subscribe and snapshot, and every action is REST」（`ws.go:278`）——**所有動作都是 REST**（POST /api/session/…），這條是 r 某轮把 ws send 那條第二實作拆掉之後的規矩。
- **`snapshot` 重播**：hub 每個 session 留最近 **400 條** event（`hub.go:19-27`），重連時逐條 push。两条關鍵規则：① **已結束的 stream 從不被重播**（`finishedStream`+`dropStream`，`:48-53`，因為重載的頁面該從 GET 拿到 committed entry）；② 佇列（512）塞不下的客戶端**直接被關**（`:72-75`），宁可讓它重連。
- 降級路徑：SSE `/api/session/{id}/events`（event 名 `agent`）→ 再降 polling 1.5s（讀 session 後**合成** `state` 幀與 `removed` 幀餵同一個 `handleEvent`）。

## 四、生命週期

**連線**：connect → `hello` → 客戶端立刻 `subscribe:""`（收全部，跨 session 的靠 `handleEvent` 開頭那段分流）+ `snapshot:<目前看的 session>` → 事件流，每拍 heartbeat 對記帳 → 連 2 拍沒心跳就主動關（`armBeatAlarm`）→ 重連延遲 0.5s×n（cap 10s）→ 3 次失敗降 SSE → 8 次降 polling。

**一輪對話（這是你問的主線）**：

```
POST /api/session/<id>/send
 └─ entry(user)                     session.go:581
 └─ state{phase:"running"}
    ├─ delta{streamId, content|reasoning|toolCall}   ×N      loop.go:738
    ├─ tool_call{run running} → tool_result{ok|error|parked} → entry(tool)  loop.go:870/912·968/969
    │   （tool 幀不帶 streamId；assistant 的 buffer 繼續留著）
    ├─ … 下一輪 turn
 └─ entry(assistant, streamId)       loop.go:526   ← 先落地
 └─ delta{streamId, done:true}       loop.go:641   ← 再關流（hub 此時把該 stream 的 deltas 抽出 replay ring）
 └─ state{phase:"idle"}
```

前端對齊這個順序：`streamStart`（首個 delta）→ `streamDelta` →（`toolCall` 插 chip）→ **`claimStream`（這刀：在 entry 幀就地升級成 `entry:<id>`）** → `streamFinish`（done 只收氣泡）。順序是反不得的：entry 在前、done 在後，所以「done 落地時 buffer 一定還在」。

**其他週期**：compaction = `request`(閘門) → `compaction`(stage/attempt) → `request_done` → `entry`(compaction) → `state`；等待／掛起 = `info` + `state{wakeAt,queued}` +（新訊息落地時）`entry`(held)；工具政策 = `tools` + `state`；回滾 = `removed` + `state`。

## 五、那個計數偏差（我上一則答錯了的地方）

```js
if (typeof msg.seq === 'number') applied++;        // transport.js:106
const expected = msg.seq - counterBase;            //             :174
if (expected > actual) ahead++; else ahead = 0;    //             :178
```

`expected` 量的是「**標尺之後**發佈了幾幀」，`actual` 卻把「標尺之後任何進過 `handleEvent` 的幀」都算進去，於是兩處灌水：

1. **`hello` 的 `seq` 是 `0`**（它不經 `mgr.publish`，而 `Seq` 沒有 `omitempty`）⇒ 每條連線固定 `+1`。
2. **`snapshot` 重播的舊幀**：它們的 `seq` 比標尺舊，但照樣 `applied++` ⇒ 重連一次再 `+k`。

結果：`expected > actual` 要成立，得**累積少掉 2 幀**（重連過則要 `k+2` 幀）才報得出來。也就是說——

- 我上一則說的「server 丟一幀／client parse 掛一幀 ⇒ beat 會抓」，**只在一窗口少 ≥2 幀時成立**；
- 因此「**少一幀**（entry 或 done 被丢）⇒ 留孤兒卡／留兩張卡」**重新變成真成因**，而且是 beat 天生看不見的那種；
- 修法一行：`if (typeof msg.seq === 'number' && msg.seq > counterBase) applied++;`——只數「標尺之後」的幀，hello（0）與重播舊幀自然被排除；daemon 重啟那條分支（`state.seq < counterBase` ⇒ 重鋪標尺）在新規則下兩邊同時歸零，仍然自洽。

---

要不要我接兩件事：① 把這份整理成 `_notes/events-and-frames.en.md`（連同第五節的偏差與重連時該怎麼驗證）；② 那行計數修正單獨一刀 `[web] count only post-ruler frames in the beat`，附一個「丢 1 幀也要能自癒」的可重複實驗。