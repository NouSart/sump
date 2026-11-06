"""Go 侧 protocol 包产出的**业务载荷**字节夹具（跨语言锚点）。

由旧夹具（Go 一次性生成后固化）拆出 payload 部分；线路容器已从 v1 信封
换成 SPv2.1 的 Frame body，但这批字节仍是 Go 编码器的真实输出。
本服务**双向**使用它们：断言 Python 能解出同样字段（Go 编码 → Python 解码），
并用同样的输入反向重编码比对（Python 编码 → 与夹具一致，见 test_protocol.py）。
任一方向的键名 / 结构漂移都会被抓住。
"""

GO_REGISTER_BODY = (
    "h6RuYW1lpXN2Yy1hpGFkZHK2d3M6Ly8xMjcuMC4wLjE6OTk5OS93c6V0aXRsZablkJHph4+rZGVzY3JpcHRpb26s5aS55YW35pyN"
    "5YqhqHByb3ZpZGVzkYOmYWN0aW9upHBpbmemb3V0cHV0pHBvbmekdG9vbMOoc2V0dGluZ3ORgqNrZXmsZGVtby5zZXR0aW5np2Rl"
    "ZmF1bHSiNW2saGVhcnRiZWF0X21zzTqY"
)
GO_ROSTER_BODY = (
    "gqhzZXJ2aWNlc5GEpG5hbWWlc3ZjLWGkYWRkcrZ3czovLzEyNy4wLjAuMTo5OTk5L3dzq2Rlc2NyaXB0aW9urOWkueWFt+acjeWK"
    "oaxoZWFydGJlYXRfbXPNOpiocmV2aXNpb24D"
)
GO_HEARTBEAT_BODY = "gaZzdGF0dXOib2s="
GO_SETTINGS_BODY = (
    "g6VjYXVzZatzZXRfc2V0dGluZ6dzZXJ2aWNlpXN2Yy1hp3Jlc3VsdHOSg6NrZXmwdG9vbC5ub3RpZnlfbW9kZaV2YWx1ZaViYXRj"
    "aKJva8ODo2tleaVnaG9zdKJva8KlZXJyb3LZMuacqumAgei+vjog5pyN5Yqh5LiN5Zyo5ZCN5YaM77yI5Y+v6IO95bey56a757q/"
    "77yJ"
)
