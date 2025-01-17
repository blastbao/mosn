package shm







// MOSN 用共享内存来存储 metrics 信息。
//
// MOSN 用 mmap 将文件映射到内存，在内存数组之上封装了一层关于 metrics 的存取逻辑，
// 实现了 go-metrics 包的关于 metrics 的接口，通过这种方式组装出了一种基于共享内存的 metrics 实现供 MOSN 使用。




// 为什么要这么辛苦封装共享内存来保存 metrics 值？为什么不直接使用堆空间来做呢？
//
//  mmap 的共享内存是可以被多个 MOSN 进程共用的。
//
//	例如 MOSN 支持跨容器热重启的场景，基于内存共享的 metrics 可以保证热重启过程中不出现指标抖动而造成监控异常。
//	而且这个文件可以看作是一种文件格式，在任何时候都可以被持久化保存和提取分析使用的。
//
//	当你不需要这个功能时，可以关闭内存共享 metrics 的配置即可，
//	MOSN 会 fallback 到 go-metrics 的实现，该实现就是通过堆分配内存保存 metrics 信息。
//
//  最后，不鼓励在 Go 里面使用共享内存，除非你有明确的使用场景，例如 MOSN 热升级场景下的 metrics 共享。
