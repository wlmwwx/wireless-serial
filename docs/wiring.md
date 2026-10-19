# 接线指南

本文说明 ESP32-C3 与目标硬件（MCU、开发板、传感器等）的 UART 接线、电平
匹配、自动下载电路与供电注意事项。

> 默认引脚来自固件 Kconfig 默认值：UART1 TX=GPIO4、RX=GPIO5；
> **RTS → 目标 RESET（GPIO3）**、**DTR → 目标 BOOT/IO0（GPIO10）**，均低有效。
> 若你在 `idf.py menuconfig` 顶层 “Wireless Serial” 菜单下改过选项，以实际
> 配置为准。

## 1. 最小接线（3.3V 目标）

只传数据时，至少需要 3 根线：

```
ESP32-C3                    目标硬件（3.3V TTL UART）
┌──────────┐                ┌──────────────────────┐
│ GPIO4 TX ├───────────────►│ RX                   │
│ GPIO5 RX │◄───────────────┤ TX                   │
│ GND      ├────────────────┤ GND                  │
└──────────┘                └──────────────────────┘
      （可选）GPIO3  ────────► EN / RESET（低有效）
      （可选）GPIO10 ────────► BOOT / IO0（低有效）
```

要点：

1. **TX ↔ RX 交叉**：ESP32-C3 的 TX 接目标 RX，ESP32-C3 的 RX 接目标 TX。
2. **必须共地**：两块板子的 GND 必须相连，否则电平参考不一致，表现为
   乱码或完全无数据。
3. 不要把目标板的 5V TX 直接接到 ESP32-C3 的 GPIO（非 5V 容忍）。
4. 数据线尽量短；超过 20–30 cm 时建议双绞 + 就近 GND，并降低波特率。

## 2. 引脚表

| ESP32-C3 引脚 | 方向 | 连接到 | 说明 |
|---------------|------|--------|------|
| GPIO4 | 输出 | 目标 RX | UART1 TX，默认 115200 8N1 |
| GPIO5 | 输入 | 目标 TX | UART1 RX，内部可配上拉（按需） |
| GND | — | 目标 GND | 必须连接 |
| GPIO3 | 输出 | 目标 EN/RESET | RTS 映射，低电平复位；未接线无副作用 |
| GPIO10 | 输出 | 目标 BOOT/IO0 | DTR 映射，低电平进入下载模式；未接线无副作用 |
| 3V3 | 电源 | 目标 VCC（小电流） | 仅在小电流、确认目标板 3.3V 供电时使用 |
| 5V/VIN | 电源 | 目标 5V（视开发板） | 由 USB 供电时可用，注意总电流 |

> 引脚定义可在固件 Kconfig 中修改；若目标板使用其他 UART 引脚，请同步
> 修改接线与 menuconfig，避免与 Strapping 引脚（GPIO2/8/9）冲突。

## 3. 电平匹配

ESP32-C3 的 GPIO 是 **3.3V 电平**，不是 5V 容忍。

### 3.1 目标为 3.3V TTL（最常见）

直接相连，无需转换。若目标板 UART 口带 5V 上拉，请确认不会把 5V 灌入
ESP32-C3。

### 3.2 目标为 5V TTL（Arduino Uno、部分传感器）

```
ESP32-C3 TX (3.3V) ──► 目标 RX（5V TTL 通常可识别 3.3V 高电平）
ESP32-C3 RX ◄── 分压/电平转换 ── 目标 TX（5V）
```

* 推荐使用双向电平转换芯片：TXB0108、TXS0108E、74LVC245（3.3V 侧供电）。
* 简易方案（仅低速、单向）：目标 TX → 1kΩ → ESP32-C3 RX，RX 到 GND 接
  2kΩ，分压后约 3.3V。**不推荐长期使用**，且会降低速率与抗干扰能力。
* 绝不能把 5V 直接接到 ESP32-C3 的 RX/TX。

### 3.3 RS232（±12V，DB9）

必须经电平转换，不能直连：

```
目标 RS232 TX ──► MAX3232 ──► ESP32-C3 RX (3.3V)
ESP32-C3 TX ──► MAX3232 ──► 目标 RS232 RX
（MAX3232 用 3.3V 供电，外围 4 个 0.1µF 电荷泵电容）
```

### 3.4 RS485（差分，半双工）

需要 RS485 收发器（MAX485/SP3485/MAX13487 等）：

```
ESP32-C3 TX ──► DI      A ────── 目标 A
ESP32-C3 RX ◄── RO      B ────── 目标 B
GPIO? ────────► DE/RE（方向控制）
```

* **当前固件不提供 DE/RE 自动方向控制**（路线图）。可行方案：
  * 使用**自动方向**收发器（如 MAX13487、SP3485 + 自动换向电路），无需
    GPIO 控制；
  * 或外接方向控制电路（如 555/比较器方案）。
* RS485 总线两端需 120Ω 终端电阻，长距离时注意共地与浪涌保护。

## 4. 自动下载/复位电路（DTR/RTS → EN/IO0）

wserial 可通过 WSP `LINE_CTRL` 与 `RESET_TARGET` 控制两个 GPIO，用于
复位 Arduino/ESP 类目标板或进入下载模式。

### 4.1 约定

| 主机信号 | ESP32-C3 默认引脚 | 目标信号 | 有效电平 |
|----------|-------------------|----------|----------|
| DTR | GPIO10 | BOOT / IO0 | 低有效 |
| RTS | GPIO3 | EN / RESET | 低有效 |

> 这是**复位目标板**用的，不是给 ESP32-C3 自己下载固件用的（ESP32-C3
> 烧录走 USB-JTAG/Serial 或 UART0，见 [firmware-build.md](firmware-build.md)）。

### 4.2 推荐：ESP32 自动下载电路（双三极管）

```
               3.3V
                 │
        ┌────────┴────────┐
        │                 │
       R1 10k            R2 10k
        │                 │
 DTR ───┴──► B(Q1)   RTS ─┴──► B(Q2)
        C(Q1)─┐            C(Q2)─┐
              │                  │
            EN/IO0            EN/IO0   ← 按目标板原理图接
              │                  │
             GND                GND
```

* 两个 NPN/PNP 组合实现「DTR/RTS 任一拉低都触发复位，二者组合进入下载
  模式」的标准电路（NodeMCU/ESP32 DevKit 同款）。
* 目标板已有 USB 下载电路时，可只接 EN 或只接 BOOT，避免冲突。

### 4.3 Arduino 类目标

* 按本设备的约定接线：**RTS（GPIO3）→ 目标 RESET**、**DTR（GPIO10）→
  目标 GPIO0(BOOT)**。注意这与 Arduino 官方 USB 串口芯片「DTR 复位、RTS
  接 GPIO0」的命名相反，照抄官方电路会导致复位/下载信号对调。
* 复位线可串 100nF 电容做低脉冲（可选）；本设备也能通过
  `wserial run --reset-on-start` 主动发送 120ms 复位脉冲，便于 IDE
  打开串口时自动复位目标。

> 注意：GPIO3/GPIO10 为普通 GPIO；若目标板不需要复位控制，可以不接线，
> 对应 Kconfig 也可关闭映射，避免误动作。

## 5. 供电与浪涌保护

1. **优先给目标板独立供电**（USB 或专用电源），ESP32-C3 的 3V3 只适合
   几十 mA 的小负载；电机、继电器、LED 灯带等必须独立供电。
2. **共地**：独立供电时，电源地必须与 ESP32-C3 的 GND 相连。
3. **浪涌/ESD**：目标板与 ESP32-C3 之间若走线较长或环境恶劣，建议：
   * TX/RX 串 22–100Ω 电阻限流；
   * 对地加 TVS（如 SMF05C）或 ESD 二极管；
   * 电源入口加磁珠/去耦电容（100nF + 10µF）。
4. **热插拔**：UART 线热插拔可能引入瞬态，尽量断电后接线。
5. **电流预算**：ESP32-C3 WiFi 发射瞬时电流可达约 300 mA，供电要留余量，
   否则会出现反复重启。

## 6. 接线检查清单

| 检查项 | 正确做法 | 常见错误 |
|--------|----------|----------|
| TX/RX | 交叉连接 | TX 接 TX，导致收不到数据 |
| GND | 共地 | 只接 TX/RX，表现为乱码 |
| 电平 | 3.3V TTL / 电平转换 | 5V 直连烧毁 GPIO |
| 波特率 | 与目标一致（默认 115200） | 目标为 9600 但未改配置 |
| 校验/停止位 | 与目标一致（默认 8N1） | 目标 7E1 导致乱码 |
| 复位线 | 可选，低有效 | 电平极性接反导致目标一直复位 |
| 供电 | 独立电源 / 共地 | 用 3V3 带大电流负载导致重启 |
| 引脚冲突 | 避开 Strapping 与 Flash 引脚 | 使用 GPIO11–17（SPI Flash）导致无法启动 |

## 7. 常见目标板示例

```
Arduino Uno（5V TTL）
  ESP32-C3 GPIO4  → 电平转换 → Uno RX(0)
  ESP32-C3 GPIO5  ← 分压/转换 ← Uno TX(1)
  ESP32-C3 GND    → Uno GND
  （Arduino IDE 下载时还会用 DTR 复位，Uno 板载 USB 串口芯片负责）

STM32 / 树莓派 Pico（3.3V TTL）
  ESP32-C3 GPIO4 → 目标 RX
  ESP32-C3 GPIO5 ← 目标 TX
  ESP32-C3 GND   → 目标 GND

ESP32 系列目标（3.3V + 自动下载）
  GPIO4/GPIO5 交叉接 TX/RX
  RTS(GPIO3)  → 目标 EN
  DTR(GPIO10) → 目标 IO0
  GND 共地
```

## 8. 相关文档

* [protocol.md](protocol.md) — DTR/RTS、BREAK、UART_CFG 的协议语义
* [architecture.md](architecture.md) — 数据流与缓冲
* [troubleshooting.md](troubleshooting.md) — 乱码、无数据、复位异常
* [faq.md](faq.md) — 电平与兼容性常见问题
