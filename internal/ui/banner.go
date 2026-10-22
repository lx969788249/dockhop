package ui

// logo 是程序开头显示的名字（字符画）。
const logo = `  ____             _    _   _
 |  _ \  ___   ___| | _| | | | ___  _ __
 | | | |/ _ \ / __| |/ / |_| |/ _ \| '_ \
 | |_| | (_) | (__|   <|  _  | (_) | |_) |
 |____/ \___/ \___|_|\_\_| |_|\___/| .__/
                                   |_|`

// Tagline 是一句话介绍。
const Tagline = "一条链接，把 Docker 服务连同数据迁到新服务器"

// Banner 是程序开头的名字和版本。终端里显示字符画 logo；输出到日志、管道时只写一行，
// 不让字符画混进日志。
func Banner(version string, tty bool) string {
	if !tty {
		return "DockHop " + version + "\n"
	}
	return "\x1b[1;36m" + logo + "\x1b[0m\n \x1b[1mDockHop " + version + "\x1b[0m · " + Tagline + "\n\n"
}
