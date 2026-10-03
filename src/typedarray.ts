/**
 * src/typedarray.ts —— TypedArray 的原地扩权工具
 *
 * 背景：构建期要往 Int32Array 里追加 (term槽位, docId) 对，百万文档下会到
 * 几百万 int。常见的"翻倍 + 新建 + set()" 写法有一个隐蔽的内存问题：
 *
 *     新建 next(2N)  ->  set 旧数据(N)  ->  旧数组要等 GC 才释放
 *
 * 追加过程中同时存活着 2N + N 两份 backing store，V8 又可能延迟回收，
 * 于是 RSS 峰值接近真实数据量的 3~4 倍。百万文档时这就是几百 MB 的虚高。
 *
 * `ArrayBuffer.prototype.transfer(newByteLength)`（ES2024）能**零拷贝**地
 * 换一个更大的 backing store：数据原地不动，旧的那份立即失效可被回收，
 * 同一时刻只有一份存活。这里用它实现"扩容量但不复制数据"。
 */

/** 支持 transfer() 的 ArrayBuffer 形态。 */
interface ResizableBuffer {
  byteLength: number;
  transfer(newByteLength: number): ArrayBuffer;
}

/**
 * 把 TypedArray 的容量原地扩到 nextCapacity，**不复制已有数据**。
 *
 * @param view        当前的 Int32Array 视图
 * @param nextCapacity 目标容量（元素数，必须 >= 当前元素数）
 * @returns 容量为 nextCapacity 的新视图（前 nextCapacity 个元素内容不变）
 */
export function growInPlace(view: Int32Array, nextCapacity: number): Int32Array {
  const byteLength = nextCapacity * Int32Array.BYTES_PER_ELEMENT;
  const buf = view.buffer as ArrayBuffer & Partial<ResizableBuffer>;

  if (typeof buf.transfer !== "function") {
    // 运行环境不支持 transfer()：退回普通拷贝（内存略高，行为仍然正确）
    const next = new Int32Array(nextCapacity);
    next.set(view);
    return next;
  }

  // 零拷贝换 backing store：内容保留，长度由新 byteLength 决定
  return new Int32Array(buf.transfer!(byteLength));
}
