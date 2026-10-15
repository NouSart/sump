"""Go 侧 protocol 包产出的字节夹具（跨语言锚点）。

与 frontend/tests/protocol.test.ts 使用的同一批 base64（由 Go 程序一次性生成后固化）：
本服务解码这些字节并逐字段断言——任何一侧的键名 / 编码漂移都会立刻被抓住。
反向锚定（Python 编码 → Go 解码）由根 tests/ 的跨语言真进程 e2e 承担。
"""

GO_REGISTER = (
    "h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVnaXN0ZXKkZnJvb"
    "aVzdmMtYaJ0b69zZXR0aW5ncy1jZW50ZXKncGF5bG9hZIakbmFtZaVzdmMtYaRhZGRytndzOi8vMTI3LjAuMC4xOjk5"
    "OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhqHByb3ZpZGVzkYOmYWN0aW9upHBpbmemb3V0cHV0pHBvbmekdG9v"
    "bMOoc2V0dGluZ3ORgqNrZXmsZGVtby5zZXR0aW5np2RlZmF1bHSiNW2saGVhcnRiZWF0X21z0wAAAAAAADqY"
)
GO_ROSTER = (
    "h6F2AaJpZLowMUZJWFJPU1RFUjAwMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0ypHR5cGWmcm9zdGVypGZyb22v"
    "c2V0dGluZ3MtY2VudGVyonRvpXN2Yy1hp3BheWxvYWSCqHNlcnZpY2VzkYSkbmFtZaVzdmMtYaRhZGRytndzOi8v"
    "MTI3LjAuMC4xOjk5OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhrGhlYXJ0YmVhdF9tc9MAAAAAAAA6mKhy"
    "ZXZpc2lvbtMAAAAAAAAAAw=="
)
GO_RESPONSE = (
    "h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVzcG9uc2WkZnJv"
    "ba9zZXR0aW5ncy1jZW50ZXKidG+lc3ZjLWGncGF5bG9hZIKib2vDpGRhdGGBqHNlcnZpY2VzkYKnc2VydmljZaVz"
    "dmMtYahzZXR0aW5nc5GEo2tleaxkZW1vLnNldHRpbmeldmFsdWWjMTBtp2RlZmF1bHSiNW2qb3ZlcnJpZGRlbsM="
)
GO_HEARTBEAT = (
    "h6F2AaJpZLowMUZJWEhFQVJUQkVBVDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0zpHR5cGWpaGVhcnRiZWF0pGZy"
    "b22lc3ZjLWGidG+vc2V0dGluZ3MtY2VudGVyp3BheWxvYWSBpnN0YXR1c6Jvaw=="
)
GO_SETTINGS = (
    "h6F2AaJpZLowMUZJWFNFVFRJTkdTMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS00pHR5cGWoc2V0dGluZ3OkZnJv"
    "ba9zZXR0aW5ncy1jZW50ZXKidG+lc3ZjLWGncGF5bG9hZIOlY2F1c2Wrc2V0X3NldHRpbmenc2VydmljZaVzdmMt"
    "YadyZXN1bHRzkoOja2V5sHRvb2wubm90aWZ5X21vZGWldmFsdWWlYmF0Y2iib2vDg6NrZXmlZ2hvc3Sib2vCpWVy"
    "cm9y2TLmnKrpgIHovr46IOacjeWKoeS4jeWcqOWQjeWGjO+8iOWPr+iDveW3suemu+e6v++8iQ=="
)
