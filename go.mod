module restreminder

// 故意停在 1.20 而不是 1.21.3：
// Go 1.21 起编译器的最低要求是 Windows 10/Server 2016，编出来的 exe 在
// Win7/Win8 上直接起不来。项目本身没有用到 1.21 的语言特性，
// 把语言版本钉在 1.20，用 Go 1.20 工具链就能编出兼容 Win7/Win8 的版本。
go 1.20
