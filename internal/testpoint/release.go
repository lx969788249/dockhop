//go:build !testpoint

package testpoint

// FromEnv 在发布的程序里什么都不读：测试用的设置都是零值，照常运行（测试版见 env.go）。
func FromEnv(func(string) string) Settings { return Settings{} }
