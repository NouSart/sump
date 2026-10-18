"""Go 侧 protocol 包产出的字节夹具（跨语言锚点）。

由 Go 程序一次性生成后固化（同一批 base64 也给了 frontend/tests/protocol.test.ts 一份）。
本服务**双向**使用它们：解码后逐字段断言（Go 编码 → Python 解码），
并用同样的输入反向重编码比对（Python 编码 → 与夹具一致，见 test_protocol.py 末组用例）。
任一方向的键名 / 结构漂移都会被抓住。
"""

GO_REGISTER = (
    "h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVnaXN0ZXKkZnJvbaVzdmMtYaJ0"
    "b69zZXR0aW5ncy1jZW50ZXKncGF5bG9hZIekbmFtZaVzdmMtYaRhZGRytndzOi8vMTI3LjAuMC4xOjk5OTkvd3OldGl0bGWm5ZCR"
    "6YePq2Rlc2NyaXB0aW9urOWkueWFt+acjeWKoahwcm92aWRlc5GDpmFjdGlvbqRwaW5npm91dHB1dKRwb25npHRvb2zDqHNldHRp"
    "bmdzkYKja2V5rGRlbW8uc2V0dGluZ6dkZWZhdWx0ojVtrGhlYXJ0YmVhdF9tc9MAAAAAAAA6mA=="
)
GO_ROSTER = (
    "h6F2AaJpZLowMUZJWFJPU1RFUjAwMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0ypHR5cGWmcm9zdGVypGZyb22v"
    "c2V0dGluZ3MtY2VudGVyonRvpXN2Yy1hp3BheWxvYWSCqHNlcnZpY2VzkYSkbmFtZaVzdmMtYaRhZGRytndzOi8v"
    "MTI3LjAuMC4xOjk5OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhrGhlYXJ0YmVhdF9tc9MAAAAAAAA6mKhy"
    "ZXZpc2lvbtMAAAAAAAAAAw=="
)
GO_RESPONSE = (
    "h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVzcG9uc2WkZnJvba9z"
    "ZXR0aW5ncy1jZW50ZXKidG+lc3ZjLWGncGF5bG9hZIKib2vDpGRhdGGBqHNlcnZpY2VzkYKnc2VydmljZaVzdmMtYahz"
    "ZXR0aW5nc5GFo2tleaxkZW1vLnNldHRpbmeldmFsdWWjMTBtp2RlZmF1bHSiNW2qb3ZlcnJpZGRlbsOmYWN0dWFsoA=="
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
