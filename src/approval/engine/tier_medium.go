package engine

// mediumBlocked 是"中"档仍然拦下的命令名：删除与改文件、改系统状态、提权、装软件、
// 联网取送与网络扫描、改账号、跑解释器（被管道喂进去那一类）。
//
// 中档的思路是"默认放行、黑名单兜底"：这里没列到的东西会被自动批准，所以名单要往宽里列。
// 名单的下限由「工作区」档守住——凡是被更松的 workspaceBlocked 拦下的系统级动作
// （提权、装系统软件、联网外发与扫描、改账号、改系统状态、终止进程），中档也必须拦；
// 两边唯一允许的差集是"工作区内的文件操作"（rm/mv/cp/chmod 那一类：中档拦、工作区档刻意放行）。
// 这条不变式由 tier_lists_test.go 机器校验，不靠两边的注释互相承诺——历史上就是这么漂移的。
// 注意它与 policy 的 deny_commands 不是一回事：deny 是硬拒（POLICY_DENY，任何档位都拒），
// 这里是"不自动放行"（转人工，你仍然可以点批准）。
var mediumBlocked = map[string]string{
	"rm": "删除文件", "rmdir": "删除目录", "ri": "删除文件", "del": "删除文件", "erase": "删除文件",
	"rd": "删除目录", "shred": "彻底删除文件", "truncate": "截断文件",
	"mv": "移动或覆盖文件", "cp": "复制并覆盖文件", "install": "安装文件",
	"chmod": "改文件权限", "chown": "改文件归属", "chgrp": "改文件归属", "attrib": "改文件属性",
	"mkfs": "格式化磁盘", "dd": "直接写块设备", "fdisk": "改分区", "parted": "改分区", "diskpart": "改分区",
	"sudo": "提权执行", "su": "切换用户", "doas": "提权执行", "runas": "提权执行",
	"apt": "装软件", "apt-get": "装软件", "yum": "装软件", "dnf": "装软件", "pacman": "装软件",
	"brew": "装软件", "choco": "装软件", "winget": "装软件", "scoop": "装软件", "snap": "装软件",
	"curl": "联网取送文件", "wget": "联网取送文件", "scp": "联网取送文件", "rsync": "联网取送文件",
	"ssh": "远程登录", "telnet": "远程登录", "nc": "联网取送文件", "ncat": "联网取送文件", "ftp": "联网取送文件",
	"systemctl": "改系统服务", "service": "改系统服务", "sc": "改系统服务", "reg": "改注册表",
	"shutdown": "开关机", "reboot": "开关机", "halt": "开关机", "poweroff": "开关机",
	"kill": "终止进程", "killall": "终止进程", "pkill": "终止进程", "taskkill": "终止进程",
	"crontab": "改定时任务", "at": "改定时任务", "useradd": "改系统账户", "userdel": "改系统账户",
	"passwd": "改账户密码", "net": "改系统账户或服务",

	// 下面五组是补齐的缺口：「工作区」档（设计上比中档更松）早就拦着这些动作，
	// 中档却放行了——档位名字听着更严、名单反而更松，就是这次要修的倒挂。
	// 共同点：没有一条是"日常开发动作"，全都是改这台机器或把东西搬出去。

	// 提权：pkexec 走 Polkit 拿 root，和 sudo / doas / runas 是同一件事。
	"pkexec": "提权执行",

	// 装系统软件：zypper（SUSE）、apk（Alpine）、msiexec（Windows 安装器）改的是系统环境，
	// 与 apt / yum / brew / winget 同类；项目内装依赖另走下面的包管理器子命令判定。
	"zypper": "装软件", "apk": "装软件", "msiexec": "装软件",

	// 外发拷贝：sftp 是 scp 的另一条通道，tftp 连认证都没有——都是把东西搬出这台机器。
	"sftp": "联网取送文件", "tftp": "联网取送文件",

	// 网络扫描：nmap 主动探测别人的机器，不是"取一份文件"那种一次性传输，中档没理由放行。
	"nmap": "扫描网络",

	// 改账号：usermod 改既有条目（加组、改 shell、锁定），和 useradd / userdel 同一个风险面。
	"usermod": "改系统账户",
}

// mediumInterpreters 是"执行器"：管道右侧出现它们且不带脚本文件时，等于把上一段的输出
// 直接执行（curl … | sh 那一类）。命令名本身不在黑名单上（python x.py 是正常开发动作），
// 只有"被喂管道"这个位置才危险。
var mediumInterpreters = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"pwsh": true, "powershell": true, "cmd": true,
}

// mediumPackageManagers 是"要看子命令才知道在做啥"的包管理器：
// `npm run build` 是构建（中档放行），`npm install` 才是装包（中档拦）。
//
// 一刀切按命令名拦会把"构建"也挂起来——那正是用户抱怨的一堆点击。
var mediumPackageManagers = map[string]bool{
	"npm": true, "pnpm": true, "yarn": true, "pip": true, "pip3": true, "gem": true,
}

// mediumInstallVerbs 是这些包管理器里"动依赖/动全局环境"的子命令。
var mediumInstallVerbs = map[string]bool{
	"install": true, "i": true, "add": true, "ci": true, "uninstall": true, "remove": true,
	"rm": true, "un": true, "unlink": true, "update": true, "upgrade": true, "up": true,
	"publish": true, "global": true, "link": true, "dedupe": true, "prune": true,
}
