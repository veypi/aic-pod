import AppKit
let app = NSApplication.shared
app.setActivationPolicy(.accessory)
let window = NSWindow(contentRect: NSRect(x: 150, y: 150, width: 420, height: 180), styleMask: [.titled, .closable], backing: .buffered, defer: false)
window.title = "AIC UI protocol fixture"
let text = NSTextField(frame: NSRect(x: 25, y: 100, width: 340, height: 28))
text.setAccessibilityLabel("Fixture name")
window.contentView!.addSubview(text)
class Handler: NSObject {
 var count = 0
 @objc func click(_ sender: Any?) { count += 1 }
}
let handler = Handler()
let button = NSButton(title: "Fixture save", target: handler, action: #selector(Handler.click(_:)))
button.frame = NSRect(x: 25, y: 40, width: 160, height: 32)
window.contentView!.addSubview(button)
window.orderBack(nil)
let output = CommandLine.arguments[1]
Timer.scheduledTimer(withTimeInterval: 0.03, repeats: true) { _ in
 if let data = try? JSONSerialization.data(withJSONObject: ["value":text.stringValue, "clicks":handler.count]) { try? data.write(to: URL(fileURLWithPath:output), options:.atomic) }
}
app.run()
