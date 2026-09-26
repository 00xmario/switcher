import AppKit

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

@main
struct MenuBarLayoutTest {
    static func main() {
        let now = Date().timeIntervalSince1970
        var cases: [[UsageWindow]] = [
            [UsageWindow(label: "Weekly", used_percent: 3,
                resets_at: now + 5 * 86400 + 3 * 3600)],
            [
                UsageWindow(label: "Go · Session", used_percent: 4,
                    resets_at: now + 4 * 3600),
                UsageWindow(label: "Go · Weekly", used_percent: 47,
                    resets_at: now + 3 * 86400 + 2 * 3600),
                UsageWindow(label: "Go · Monthly", used_percent: 65,
                    resets_at: now + 12 * 86400 + 8 * 3600),
            ],
        ]
        if let raw = ProcessInfo.processInfo.environment["SWITCHER_USAGE_WINDOWS"],
           let data = raw.data(using: .utf8),
           let live = try? JSONDecoder().decode([[UsageWindow]].self, from: data) {
            cases.append(contentsOf: live)
        }
        let contentWidth = menuWidth - 2 * edgeInset
        for windows in cases {
            let lineHeight: CGFloat = 15
            let rowHeight = CGFloat(9 + 15 + 4 + 8) + CGFloat(windows.count) * lineHeight
            let titleY = rowHeight - 9 - 15
            let textX = cardInset
            for showBars in [true, false] {
                let layout = usageLayout(windows: windows, titleY: titleY,
                    lineWidth: contentWidth - textX - cardInset,
                    contentWidth: contentWidth, textX: textX,
                    cardInset: cardInset, lineHeight: lineHeight, titleGap: 4,
                    showBars: showBars)
                for line in layout.lines {
                    guard line.value.width >= textWidth("100% left", font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)) else {
                        fail("Percentage value clipped")
                    }
                    guard !line.label.intersects(layout.checkmark) && !line.value.intersects(layout.checkmark) else {
                        fail("Checkmark overlaps a usage line")
                    }
                    if let reset = line.resetText, let frame = line.reset {
                        let needed = textWidth(reset, font: NSFont.systemFont(ofSize: 10.5))
                        guard frame.width >= needed else {
                            fail("Truncated reset: \(reset), need \(needed), got \(frame.width)")
                        }
                        guard frame.minX >= line.value.maxX + 5 && frame.maxX <= contentWidth - cardInset else {
                            fail("Reset overlaps value or escapes card inset")
                        }
                        guard !frame.intersects(layout.checkmark) else {
                            fail("Reset overlaps active checkmark")
                        }
                    } else if line.resetText != nil || line.reset != nil {
                        fail("Reset text and frame disagree")
                    }
                    if showBars {
                        guard let bar = line.bar, bar.minX >= line.label.maxX + 5,
                              line.value.minX >= bar.maxX + 5,
                              bar.maxY <= line.label.maxY else {
                            fail("Inline usage bar overlaps a label or percentage")
                        }
                    } else if line.bar != nil {
                        fail("Usage bar present when disabled")
                    }
                }
                guard layout.checkmark.minY >= titleY && layout.checkmark.maxY <= titleY + 15 else {
                    fail("Checkmark is outside the account title line")
                }
            }
        }
        let noReset = usageLayout(windows: [UsageWindow(label: "Fable", used_percent: 0, resets_at: nil)],
            titleY: 27, lineWidth: contentWidth - 2 * cardInset, contentWidth: contentWidth,
            textX: cardInset, cardInset: cardInset, lineHeight: 15, titleGap: 4, showBars: true)
        guard noReset.lines[0].reset == nil && noReset.lines[0].resetText == nil else {
            fail("A window without a reset timestamp acquired a countdown")
        }
        guard exactResetTime(nil) == nil, exactResetTime(now - 10) == nil,
              let exact = exactResetTime(now + 3600), exact.contains("Provider-reported reset"),
              exact.contains(" (UTC") else {
            fail("Exact reset hover lacks local time zone or appears for an absent reset")
        }
        let longLabel = usageLayout(windows: [UsageWindow(
            label: "Very long window label from upstream", used_percent: 0,
            resets_at: now + 12 * 86400)], titleY: 27,
            lineWidth: contentWidth - 2 * cardInset, contentWidth: contentWidth,
            textX: cardInset, cardInset: cardInset, lineHeight: 15, titleGap: 4,
            showBars: true)
        guard longLabel.lines[0].bar == nil &&
              longLabel.lines[0].value.width >= textWidth("100% left", font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)) else {
            fail("Long label did not preserve the percentage column")
        }
        let base = Account(id: "a", provider: "codex", email: "private@example.com", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true, windows: [
                UsageWindow(label: "Session", used_percent: 25, resets_at: now + 3600),
                UsageWindow(label: "Weekly", used_percent: 0, resets_at: now + 7200),
                UsageWindow(label: "Past", used_percent: 50, resets_at: now - 5),
                UsageWindow(label: "Unknown", used_percent: 50, resets_at: nil),
            ]), reset_credits: ResetCredits(count: 1))
        guard bankedResetText(base.reset_credits) == "⚡ 1 banked",
              bankedResetText(nil) == nil,
              bankedResetText(ResetCredits(count: 0)) == nil else {
            fail("Banked reset count missing or shown for an empty balance")
        }
        func state(_ accounts: [Account], _ enabled: Bool) -> AppState {
            AppState(accounts: accounts, order: nil, hidden: nil, version: nil,
                update: nil, menu_usage_bars: nil, reset_notifications: enabled)
        }
        let alerts = desiredResetAlerts(state([base], true), now: Date(timeIntervalSince1970: now))
        guard alerts.count == 1, alerts[0].window == "Session", !alerts[0].id.contains("private") else {
            fail("Reset planner included unused/past/unknown windows or private identity")
        }
        let shifted = Account(id: "a", provider: "codex", email: base.email, plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Session", used_percent: 25, resets_at: now + 3700)]), reset_credits: nil)
        let newAlerts = desiredResetAlerts(state([shifted], true), now: Date(timeIntervalSince1970: now))
        guard newAlerts.count == 1, newAlerts[0].id != alerts[0].id,
              desiredResetAlerts(state([], true)).isEmpty,
              desiredResetAlerts(state([base], false)).isEmpty else {
            fail("Reset alert shift, deletion, or disable planning failed")
        }
        let many = (0..<40).map { i in
            Account(id: "account-\(i)", provider: "codex", email: "", plan: nil,
                active: false, exhausted_until: nil, usage: Usage(available: true,
                    windows: [UsageWindow(label: "Session", used_percent: 1, resets_at: now + Double(40-i)*60)]), reset_credits: nil)
        }
        let capped = desiredResetAlerts(state(many, true), now: Date(timeIntervalSince1970: now))
        guard capped.count == 32, capped[0].id.contains("account-39") else {
            fail("Reset planner did not choose the earliest 32 windows")
        }
        guard logoUsesTemplate("copilot"), logoUsesTemplate("grok"), !logoUsesTemplate("gemini"),
              let copilot = NSImage(contentsOfFile: "build/macos/logos/copilot.svg") else {
            fail("Copilot logo must load as a tintable template")
        }
        copilot.isTemplate = logoUsesTemplate("copilot")
        func logoBrightness(_ tint: NSColor) -> CGFloat {
            let view = NSImageView(frame: NSRect(x: 0, y: 0, width: 22, height: 22))
            view.image = copilot
            view.contentTintColor = tint
            guard let rep = renderBitmap(view) else { fail("Copilot mark did not render") }
            var total: CGFloat = 0
            var pixels = 0
            for y in 0..<rep.pixelsHigh {
                for x in 0..<rep.pixelsWide {
                    guard let color = rep.colorAt(x: x, y: y)?.usingColorSpace(.deviceRGB),
                          color.alphaComponent > 0.5 else { continue }
                    total += (color.redComponent + color.greenComponent + color.blueComponent) / 3
                    pixels += 1
                }
            }
            guard pixels > 0 else { fail("Copilot mark has no visible pixels") }
            return total / CGFloat(pixels)
        }
        guard logoBrightness(Palette.dark.ink) > logoBrightness(Palette.light.ink) + 0.35 else {
            fail("Copilot mark did not follow light and dark menu colours")
        }
        print("Menu bar layout and Copilot theme tint: OK")
    }
}
