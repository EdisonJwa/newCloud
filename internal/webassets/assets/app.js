// newcloud — 浏览器客户端
//
// 设计方向见 docs/0005-interface-design.md。无依赖、无打包器，三份静态资源内嵌进二进制。
//
// 贯穿全文的四条规则：
//
//  1. 用户数据只经 textContent 进 DOM。文件名、分享标签、邮箱都来自他人，拼进 HTML 就是
//     对浏览者的存储型 XSS。
//  2. 服务器决定什么可以预览与生成缩略图。客户端读 API 返回的 `preview` 字段，绝不按扩展名
//     自行判断后与服务器唱反调。
//  3. 界面不猜测能力。状态条只展示 API 真实返回的配额与布局；不编造"已同步"这类没有后端
//     支撑的状态。
//  4. 所有面向用户的字符串来自键表，且不经字符串拼接——词序随语言变化，拼起来就翻不了。

(() => {
  "use strict";

  // ---------- 国际化 ----------
  //
  // 键表而非拼接：`{count} items` 这样的模板按语言整体替换，复数走 Intl.PluralRules。
  // 数字与日期一律 Intl，不假设 12 小时制、周首日、千分位。

  const STRINGS = {
    en: {
      "app.name": "newcloud",
      "nav.files": "Files",
      "nav.shares": "Shares",
      "nav.trash": "Trash",
      "nav.activity": "Activity",
      "nav.access": "Access",
      "nav.collapse": "Collapse sidebar",
      "nav.expand": "Expand sidebar",
      "nav.signout": "Sign out",

      "head.upload": "Upload",
      "head.newFolder": "New folder",
      "head.filter": "Filter",
      "head.view": "View",
      "head.list": "List",
      "head.gallery": "Gallery",
      "head.density": "Density",
      "head.comfortable": "Comfortable",
      "head.compact": "Compact",
      "head.theme": "Theme",
      "head.themeAuto": "Auto",
      "head.themeLight": "Light",
      "head.themeDark": "Dark",
      "head.themeHighContrast": "High contrast",
      "head.palette": "Commands",
      "head.details": "Details panel",
      "head.home": "Home",

      "list.name": "Name",
      "list.size": "Size",
      "list.modified": "Modified",
      "list.marks": "Shared",

      "count.items_one": "{n} item",
      "count.items_other": "{n} items",
      "count.shown": "{shown} of {total}",
      "count.shares_one": "{n} link",
      "count.shares_other": "{n} links",
      "count.events_one": "{n} event",
      "count.events_other": "{n} events",
      "count.versions_one": "{n} version",
      "count.versions_other": "{n} versions",
      "count.links_one": "{n} link",
      "count.links_other": "{n} links",
      "count.selected_one": "{n} selected",
      "count.selected_other": "{n} selected",

      "hint.gallery": "Gallery · {n} images",
      "hint.compact": "Compact · {n} items",
      "hint.text": "List · mostly text",
      "hint.pinned": "Pinned · {view}",

      "state.emptyTitle": "This folder is empty",
      "state.emptyHint": "Drop files anywhere, or use Upload.",
      "state.noMatchTitle": "Nothing matches",
      "state.noMatchHint": "Only this folder is searched.",
      "state.clearFilter": "Clear filter",
      "state.errorTitle": "Something went wrong",
      "state.retry": "Try again",
      "state.deniedTitle": "No access",
      "state.deniedHint": "You do not have permission to view this.",
      "state.offlineTitle": "You are offline",
      "state.offlineHint": "Changes cannot be saved until the connection returns.",
      "state.trashEmpty": "Trash is empty",
      "state.trashEmptyHint": "Deleted files appear here and can be restored.",
      "state.sharesEmpty": "No shares yet",
      "state.sharesEmptyHint": "Select a file and choose Share to create a link.",
      "state.activityEmpty": "No activity",
      "state.activityEmptyHint": "Events appear here as files are touched.",
      "state.activityFiltered": "Nothing matches these filters.",
      "banner.offline": "Offline — actions are disabled.",
      "banner.quota": "Storage is full. Delete or move files to free space.",

      "status.online": "Online",
      "status.offline": "Offline",
      "status.storage": "{used} of {quota}",
      "status.storageFree": "{used} used",
      "status.layout": "{layout}",
      "status.uploading": "Uploading {done}/{total}",

      "insp.details": "Details",
      "insp.folder": "This folder",
      "insp.close": "Clear selection",
      "insp.nothing": "Nothing selected",
      "insp.nothingHint": "Select a file to see its details, versions and shares. Double-click or press Enter to open.",
      "insp.files": "Files",
      "insp.folders": "Folders",
      "insp.totalSize": "Total size",
      "insp.kind": "Kind",
      "insp.size": "Size",
      "insp.modified": "Modified",
      "insp.type": "Type",
      "insp.preview": "Preview",
      "insp.path": "Path",
      "insp.open": "Open",
      "insp.download": "Download",
      "insp.share": "Share",
      "insp.rename": "Rename",
      "insp.delete": "Delete",
      "insp.versions": "Version history",
      "insp.sharedBy": "Shared by",
      "insp.noVersions": "No earlier versions. A new version is stored each time the file is overwritten.",
      "insp.notShared": "Not shared. Use Share to create a link.",
      "insp.linkNotRecoverable": "link not recoverable",
      "insp.restore": "Restore",
      "insp.stageNone": "No inline preview for this type",
      "insp.stageError": "Preview unavailable",
      "insp.openPreview": "Open preview",
      "insp.loading": "Loading…",

      "kind.folder": "folder",
      "kind.image": "image",
      "kind.video": "video",
      "kind.audio": "audio",
      "kind.pdf": "PDF",
      "kind.text": "text",
      "kind.file": "file",

      "action.preview": "Preview",
      "action.download": "Download",
      "action.share": "Share",
      "action.rename": "Rename",
      "action.delete": "Delete",
      "action.restore": "Restore",
      "action.revoke": "Revoke",
      "action.cancel": "Cancel",
      "action.create": "Create",
      "action.done": "Done",
      "action.copy": "Copy",
      "action.undo": "Undo",
      "action.clear": "Clear",

      "dlg.newFolder": "New folder",
      "dlg.folderName": "Name",
      "dlg.rename": "Rename",
      "dlg.newName": "New name",
      "dlg.deleteTitle": "Delete {name}?",
      "dlg.deleteManyTitle": "Delete {n} items?",
      "dlg.deleteBody": "They move to the trash and can be restored from there.",
      "dlg.shareTitle": "Share {name}",
      "dlg.shareType": "Type",
      "dlg.shareLink": "Link — anyone with the URL can download",
      "dlg.shareRequest": "Request — others can upload into this folder",
      "dlg.password": "Password (optional)",
      "dlg.expires": "Expires in hours (optional)",
      "dlg.maxUses": "Maximum uses (optional)",
      "dlg.createLink": "Create link",
      "dlg.linkCreated": "Link created",
      "dlg.linkCreatedBody": "Copied to the clipboard. The token is not shown again.",
      "dlg.newPassword": "New app password",
      "dlg.label": "Label",
      "dlg.scope": "Scope",
      "dlg.scopeRW": "Read and write",
      "dlg.scopeRO": "Read only",
      "dlg.passwordOnce": "Copy this password now",
      "dlg.passwordOnceBody": "It cannot be shown again. Use your username with this value as the password.",

      "toast.uploaded_one": "Uploaded {n} file",
      "toast.uploaded_other": "Uploaded {n} files",
      "toast.uploadedSome": "Uploaded {done}, {failed} failed",
      "toast.uploading": "An upload is already running",
      "toast.deleted_one": "Moved to trash",
      "toast.deleted_other": "Moved {n} items to trash",
      "toast.renamed": "Renamed",
      "toast.folderCreated": "Folder created",
      "toast.restored": "Restored",
      "toast.revoked": "Revoked",
      "toast.shareRevoked": "Share revoked",
      "toast.restoredVersion": "Restored v{version}",
      "toast.linkCopied": "Link copied",

      "audit.action": "Action",
      "audit.result": "Result",
      "audit.since": "Time",
      "audit.allActions": "All actions",
      "audit.anyResult": "Any result",
      "audit.allowed": "Allowed",
      "audit.refused": "Refused",
      "audit.allTime": "All time",
      "audit.lastHour": "Last hour",
      "audit.last24": "Last 24 hours",
      "audit.last7": "Last 7 days",
      "audit.last30": "Last 30 days",
      "audit.export": "Export",
      "audit.loadOlder": "Load older events",
      "audit.end": "End of the log",
      "audit.personAnonymous": "anonymous",
      "audit.personViaLink": "via link",
      "audit.personSystem": "system",
      "audit.scopeTitle": "Scope",
      "audit.scopeBody": "You see what you did, plus everything that happened to your own files and links — including downloads by anonymous visitors and refused attempts. Administrators see the whole instance.",
      "audit.integrityTitle": "Integrity",
      "audit.integrityBody": "Rows are hash-chained in the order written. An edited or removed row breaks the chain, which is what the export verifies.",

      "access.title": "Connecting a client",
      "access.body": "Use an app password instead of your browser session for WebDAV, rclone or a mounted drive. Each one can be revoked on its own, so a leaked credential does not end your browser login.",
      "access.lastUsed": "Last used",
      "access.never": "never",
      "access.scope": "Scope",
      "access.label": "Label",

      "shares.caption": "Each link can be limited by password, expiry and use count. Revoke one here; the original URL cannot be shown again, because only its hash is stored.",
      "shares.target": "Target",
      "shares.kind": "Kind",
      "shares.uses": "Uses",
      "shares.expires": "Expires",
      "shares.never": "never",

      "palette.placeholder": "Type a command or file name…",
      "palette.empty": "No matches",
      "palette.files": "Files",
      "palette.commands": "Commands",
      "palette.goFiles": "Go to Files",
      "palette.goShares": "Go to Shares",
      "palette.goTrash": "Go to Trash",
      "palette.goActivity": "Go to Activity",
      "palette.goAccess": "Go to Access",
      "palette.upload": "Upload files",
      "palette.newFolder": "New folder",
      "palette.toggleTheme": "Switch theme",
      "palette.toggleDensity": "Toggle density",
      "palette.toggleRail": "Toggle sidebar",
      "palette.toggleInspector": "Toggle details panel",
      "palette.selectAll": "Select all",
      "palette.clearSelection": "Clear selection",
      "palette.hintNav": "↑↓ navigate",
      "palette.hintRun": "Enter run",
      "palette.hintClose": "Esc close",

      "preview.unavailable": "Preview unavailable",
      "preview.imageFailed": "This image could not be displayed.",
      "preview.videoFailed": "This video format is not supported by the browser.",
      "preview.audioFailed": "This audio format is not supported by the browser.",
      "preview.none": "No preview for this file type.",
      "preview.loading": "Loading…",
      "preview.loadFailed": "Could not load the file: {msg}",

      "share.download": "Download",
      "share.upload": "Upload",
      "share.continue": "Continue",
      "share.password": "Password",
      "share.wrongPassword": "Wrong password",
      "share.email": "Your email",
      "share.code": "Code",
      "share.sendCode": "Send me a code",
      "share.codeSent": "If that address was invited, a code is on its way",
      "share.codeBad": "That code was not accepted",
      "share.open": "Open",
      "share.file": "File",
      "share.slots": "Upload a file. {left} of {max} slots left.",
      "share.uploadOne": "Upload a file to send it.",
      "share.passwordProtected": "This link is protected by a password.",
      "share.emailLimited": "This link is limited to specific addresses.",
      "share.cannotRead": "This link cannot be downloaded.",
      "share.notAvailable": "Link not available",
      "share.notAvailableHint": "It may have expired, been revoked, or reached its usage limit.",
      "share.expires": "expires {date}",
      "share.sharedWith": "Shared with newcloud",
    },

    "zh-Hans": {
      "app.name": "newcloud",
      "nav.files": "文件",
      "nav.shares": "分享",
      "nav.trash": "回收站",
      "nav.activity": "活动",
      "nav.access": "访问",
      "nav.collapse": "收起侧栏",
      "nav.expand": "展开侧栏",
      "nav.signout": "退出登录",

      "head.upload": "上传",
      "head.newFolder": "新建文件夹",
      "head.filter": "筛选",
      "head.view": "视图",
      "head.list": "列表",
      "head.gallery": "画廊",
      "head.density": "密度",
      "head.comfortable": "舒适",
      "head.compact": "紧凑",
      "head.theme": "主题",
      "head.themeAuto": "跟随系统",
      "head.themeLight": "明亮",
      "head.themeDark": "暗色",
      "head.themeHighContrast": "高对比",
      "head.palette": "命令",
      "head.details": "详情面板",
      "head.home": "主页",

      "list.name": "名称",
      "list.size": "大小",
      "list.modified": "修改时间",
      "list.marks": "分享",

      "count.items_other": "{n} 项",
      "count.shown": "{shown} / {total}",
      "count.shares_other": "{n} 个链接",
      "count.events_other": "{n} 条事件",
      "count.versions_other": "{n} 个版本",
      "count.links_other": "{n} 个链接",
      "count.selected_other": "已选 {n} 项",

      "hint.gallery": "画廊 · {n} 张图片",
      "hint.compact": "紧凑 · {n} 项",
      "hint.text": "列表 · 以文本为主",
      "hint.pinned": "已钉住 · {view}",

      "state.emptyTitle": "这个文件夹是空的",
      "state.emptyHint": "可以把文件拖到窗口任意位置，或使用“上传”。",
      "state.noMatchTitle": "没有匹配项",
      "state.noMatchHint": "只搜索当前文件夹。",
      "state.clearFilter": "清除筛选",
      "state.errorTitle": "出错了",
      "state.retry": "重试",
      "state.deniedTitle": "没有权限",
      "state.deniedHint": "你没有查看此对象的权限。",
      "state.offlineTitle": "当前处于离线状态",
      "state.offlineHint": "连接恢复前无法保存更改。",
      "state.trashEmpty": "回收站是空的",
      "state.trashEmptyHint": "删除的文件会出现在这里，可以恢复。",
      "state.sharesEmpty": "还没有分享",
      "state.sharesEmptyHint": "选中文件后点“分享”即可创建链接。",
      "state.activityEmpty": "暂无活动",
      "state.activityEmptyHint": "文件被访问时，事件会出现在这里。",
      "state.activityFiltered": "没有符合筛选条件的事件。",
      "banner.offline": "离线中——操作已禁用。",
      "banner.quota": "存储空间已满。请删除或移走文件以释放空间。",

      "status.online": "在线",
      "status.offline": "离线",
      "status.storage": "{used} / {quota}",
      "status.storageFree": "已用 {used}",
      "status.layout": "{layout}",
      "status.uploading": "上传中 {done}/{total}",

      "insp.details": "详情",
      "insp.folder": "此文件夹",
      "insp.close": "清除选中",
      "insp.nothing": "未选中任何对象",
      "insp.nothingHint": "选中一个文件查看详情、版本与分享。双击或按回车打开。",
      "insp.files": "文件",
      "insp.folders": "文件夹",
      "insp.totalSize": "总大小",
      "insp.kind": "种类",
      "insp.size": "大小",
      "insp.modified": "修改时间",
      "insp.type": "类型",
      "insp.preview": "预览",
      "insp.path": "路径",
      "insp.open": "打开",
      "insp.download": "下载",
      "insp.share": "分享",
      "insp.rename": "重命名",
      "insp.delete": "删除",
      "insp.versions": "版本历史",
      "insp.sharedBy": "被分享",
      "insp.noVersions": "没有更早的版本。文件每次被覆盖时会保存一个新版本。",
      "insp.notShared": "未被分享。使用“分享”创建链接。",
      "insp.linkNotRecoverable": "链接不可再取回",
      "insp.restore": "恢复",
      "insp.stageNone": "此类型没有内联预览",
      "insp.stageError": "预览不可用",
      "insp.openPreview": "打开预览",
      "insp.loading": "加载中…",

      "kind.folder": "文件夹",
      "kind.image": "图片",
      "kind.video": "视频",
      "kind.audio": "音频",
      "kind.pdf": "PDF",
      "kind.text": "文本",
      "kind.file": "文件",

      "action.preview": "预览",
      "action.download": "下载",
      "action.share": "分享",
      "action.rename": "重命名",
      "action.delete": "删除",
      "action.restore": "恢复",
      "action.revoke": "吊销",
      "action.cancel": "取消",
      "action.create": "创建",
      "action.done": "完成",
      "action.copy": "复制",
      "action.undo": "撤销",
      "action.clear": "清除",

      "dlg.newFolder": "新建文件夹",
      "dlg.folderName": "名称",
      "dlg.rename": "重命名",
      "dlg.newName": "新名称",
      "dlg.deleteTitle": "删除 {name}？",
      "dlg.deleteManyTitle": "删除 {n} 项？",
      "dlg.deleteBody": "它们会进入回收站，可以从那里恢复。",
      "dlg.shareTitle": "分享 {name}",
      "dlg.shareType": "类型",
      "dlg.shareLink": "链接 — 拿到 URL 的人都能下载",
      "dlg.shareRequest": "投递箱 — 别人可向此文件夹上传",
      "dlg.password": "密码（可选）",
      "dlg.expires": "有效期小时数（可选）",
      "dlg.maxUses": "使用次数上限（可选）",
      "dlg.createLink": "创建链接",
      "dlg.linkCreated": "链接已创建",
      "dlg.linkCreatedBody": "已复制到剪贴板。令牌不会再次显示。",
      "dlg.newPassword": "新建应用密码",
      "dlg.label": "标签",
      "dlg.scope": "范围",
      "dlg.scopeRW": "读写",
      "dlg.scopeRO": "只读",
      "dlg.passwordOnce": "现在就复制这个密码",
      "dlg.passwordOnceBody": "它不会再次显示。使用时用户名照常，密码填这个值。",

      "toast.uploaded_other": "已上传 {n} 个文件",
      "toast.uploadedSome": "已上传 {done} 个，{failed} 个失败",
      "toast.uploading": "已有上传在进行",
      "toast.deleted_other": "已移入回收站",
      "toast.renamed": "已重命名",
      "toast.folderCreated": "文件夹已创建",
      "toast.restored": "已恢复",
      "toast.revoked": "已吊销",
      "toast.shareRevoked": "分享已吊销",
      "toast.restoredVersion": "已恢复 v{version}",
      "toast.linkCopied": "链接已复制",

      "audit.action": "动作",
      "audit.result": "结果",
      "audit.since": "时间",
      "audit.allActions": "全部动作",
      "audit.anyResult": "任意结果",
      "audit.allowed": "允许",
      "audit.refused": "拒绝",
      "audit.allTime": "全部时间",
      "audit.lastHour": "最近 1 小时",
      "audit.last24": "最近 24 小时",
      "audit.last7": "最近 7 天",
      "audit.last30": "最近 30 天",
      "audit.export": "导出",
      "audit.loadOlder": "加载更早的事件",
      "audit.end": "日志到头了",
      "audit.personAnonymous": "匿名",
      "audit.personViaLink": "通过链接",
      "audit.personSystem": "系统",
      "audit.scopeTitle": "范围",
      "audit.scopeBody": "你能看到自己的操作，以及发生在你自己文件和链接上的一切——包括匿名访客的下载和被拒绝的尝试。管理员能看到整个实例。",
      "audit.integrityTitle": "完整性",
      "audit.integrityBody": "记录按写入顺序做哈希链。任何一行被修改或删除都会断链，导出时会校验这一点。",

      "access.title": "连接客户端",
      "access.body": "WebDAV、rclone 或挂载盘请使用应用密码，而不是浏览器会话。每个都可以单独吊销，泄露一个不会影响你的浏览器登录。",
      "access.lastUsed": "最近使用",
      "access.never": "从未",
      "access.scope": "范围",
      "access.label": "标签",

      "shares.caption": "每个链接都可以限制密码、有效期与使用次数。这里可以吊销；原始 URL 无法再次显示，因为只存了它的哈希。",
      "shares.target": "目标",
      "shares.kind": "类型",
      "shares.uses": "使用",
      "shares.expires": "过期",
      "shares.never": "不过期",

      "palette.placeholder": "输入命令或文件名…",
      "palette.empty": "没有匹配项",
      "palette.files": "文件",
      "palette.commands": "命令",
      "palette.goFiles": "前往文件",
      "palette.goShares": "前往分享",
      "palette.goTrash": "前往回收站",
      "palette.goActivity": "前往活动",
      "palette.goAccess": "前往访问",
      "palette.upload": "上传文件",
      "palette.newFolder": "新建文件夹",
      "palette.toggleTheme": "切换主题",
      "palette.toggleDensity": "切换密度",
      "palette.toggleRail": "切换侧栏",
      "palette.toggleInspector": "切换详情面板",
      "palette.selectAll": "全选",
      "palette.clearSelection": "清除选中",
      "palette.hintNav": "↑↓ 移动",
      "palette.hintRun": "回车执行",
      "palette.hintClose": "Esc 关闭",

      "preview.unavailable": "预览不可用",
      "preview.imageFailed": "这张图片无法显示。",
      "preview.videoFailed": "浏览器不支持这个视频格式。",
      "preview.audioFailed": "浏览器不支持这个音频格式。",
      "preview.none": "此类型没有预览。",
      "preview.loading": "加载中…",
      "preview.loadFailed": "无法加载文件：{msg}",

      "share.download": "下载",
      "share.upload": "上传",
      "share.continue": "继续",
      "share.password": "密码",
      "share.wrongPassword": "密码错误",
      "share.email": "你的邮箱",
      "share.code": "验证码",
      "share.sendCode": "发验证码给我",
      "share.codeSent": "如果该地址在邀请名单内，验证码已发出",
      "share.codeBad": "验证码未被接受",
      "share.open": "打开",
      "share.file": "文件",
      "share.slots": "上传一个文件。还剩 {left} / {max} 个名额。",
      "share.uploadOne": "上传一个文件发送过去。",
      "share.passwordProtected": "此链接受密码保护。",
      "share.emailLimited": "此链接仅限指定邮箱访问。",
      "share.cannotRead": "此链接不可下载。",
      "share.notAvailable": "链接不可用",
      "share.notAvailableHint": "它可能已过期、已被吊销，或已达使用上限。",
      "share.expires": "{date} 过期",
      "share.sharedWith": "由 newcloud 分享",
    },
  };

  const SUPPORTED = ["en", "zh-Hans"];

  // lang 由浏览器语言决定，用户选择存 localStorage。
  let lang = (() => {
    const stored = safeGet("wc.lang");
    if (stored && SUPPORTED.includes(stored)) return stored;
    for (const tag of navigator.languages || [navigator.language || "en"]) {
      const lower = String(tag).toLowerCase();
      if (lower.startsWith("zh")) return "zh-Hans";
      if (lower.startsWith("en")) return "en";
    }
    return "en";
  })();

  const plural = new Intl.PluralRules(lang);

  // t 查表并替换占位符。
  //
  // 复数走 Intl.PluralRules，取 `key_one` / `key_other`；中文只有 other，英文按 en 规则。
  // 占位符整体替换，不做拼接。
  function t(key, params) {
    const table = STRINGS[lang] || STRINGS.en;
    let template;
    if (params && typeof params.n === "number") {
      const form = plural.select(params.n);
      template = table[`${key}_${form}`] || table[`${key}_other`] || table[`${key}_one`];
    }
    if (template === undefined) template = table[key];
    if (template === undefined) template = (STRINGS.en[key] !== undefined ? STRINGS.en[key] : key);

    if (!params) return template;
    return template.replace(/\{(\w+)\}/g, (whole, name) => {
      if (!(name in params)) return whole;
      const value = params[name];
      if (typeof value === "number") return num.format(value);
      return String(value);
    });
  }

  const num = new Intl.NumberFormat(lang);

  // ---------- 本地偏好 ----------

  function safeGet(key) {
    try {
      return localStorage.getItem(key);
    } catch {
      return null;
    }
  }

  function safeSet(key, value) {
    try {
      localStorage.setItem(key, value);
    } catch {
      /* 隐私模式下不可写，偏好仅本次有效 */
    }
  }

  // ---------- 状态 ----------

  const state = {
    me: null,
    view: "files", // files | shares | trash | audit | access
    path: "",
    entries: [],
    viewMode: "list", // list | gallery
    viewPinned: false,
    suggestion: null,
    filter: "",
    selected: new Set(),
    anchor: null,
    // 桌面默认展开：一上来就是图标条，对第一次使用的人等于没有导航。
    railExpanded: (safeGet("wc.rail") || (window.innerWidth >= 640 ? "expanded" : "collapsed")) === "expanded",
    themePref: safeGet("wc.theme") || "auto",
    density: safeGet("wc.density") || "comfortable",
    inspectorClosed: false,
    online: navigator.onLine !== false,
    detail: { path: null, versions: null, shares: null, loading: false },
    paletteOpen: false,
    audit: {
      events: [],
      offset: 0,
      total: 0,
      hasMore: false,
      actions: [],
      action: "",
      result: "",
      since: "",
      loading: false,
    },
  };

  const $ = (sel) => document.querySelector(sel);

  // refocusList asks the next render to place focus on the file listbox.
  //
  // Rows are not tab stops: the listbox is, and it tracks the active row through
  // aria-activedescendant. So a click on a row must move focus to the listbox, or the arrow
  // keys afterwards go nowhere. Every render rebuilds the DOM, so the request has to survive
  // the rebuild — hence a flag rather than a direct focus() call.
  let refocusList = false;

  function requestListFocus() {
    refocusList = true;
  }

  // ---------- DOM 辅助 ----------

  // el builds a DOM node from options.
  //
  // There is deliberately no `style` option. The application CSP is style-src 'self' with no
  // unsafe-inline, so an inline style attribute is stored in the DOM but never applied — a
  // silent failure that cost a debugging session. Use a class instead.
  function el(tag, opts = {}, children = []) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(opts)) {
      if (value == null || value === false) continue;
      if (key === "text") node.textContent = value;
      else if (key === "class") node.className = value;
      else if (key === "dataset") Object.assign(node.dataset, value);
      else if (key === "style") node.setAttribute("style", value);
      else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value === true ? "" : value);
    }
    for (const child of [].concat(children)) {
      if (child == null || child === false) continue;
      node.append(typeof child === "string" ? document.createTextNode(child) : child);
    }
    return node;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function svgIcon(name, size = 15) {
    const paths = {
      search: "M10.5 10.5l3.5 3.5M7.5 12.5a5 5 0 100-10 5 5 0 000 10z",
      files: "M3 4.5A1.5 1.5 0 014.5 3h2.4l1.2 1.5H13A1 1 0 0114 5.5v7.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4.5z",
      shares: "M6 9.5l4.5-3.5M6 7l4.5 3.5M4 9.5a1.75 1.75 0 110-3.5 1.75 1.75 0 010 3.5zm8 4.5a1.75 1.75 0 110-3.5 1.75 1.75 0 010 3.5zm0-8a1.75 1.75 0 110-3.5 1.75 1.75 0 010 3.5z",
      trash: "M3.5 5h9M6 5V3.8a.8.8 0 01.8-.8h2.4a.8.8 0 01.8.8V5M5 5l.4 8a.9.9 0 00.9.9h3.4a.9.9 0 00.9-.9L11 5",
      key: "M9.5 6.5a2.5 2.5 0 11-1.7 4.3L4.5 14H3v-1.5l3.8-3.3a2.5 2.5 0 012.7-2.7z",
      pulse: "M1.5 8h3l1.5-4 2.5 8 1.5-4h3.5",
      upload: "M8 11V3m0 0L5 6m3-3l3 3M2.5 11.5v1.5a1 1 0 001 1h9a1 1 0 001-1v-1.5",
      folderPlus: "M3 4.5A1.5 1.5 0 014.5 3h2.4l1.2 1.5H13A1 1 0 0114 5.5v7.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4.5zM8 7.5v3.5M6.25 9.25h3.5",
      refresh: "M13.5 8a5.5 5.5 0 11-1.6-3.9M13.5 2v3h-3",
      list: "M2.5 4h11M2.5 8h11M2.5 12h11",
      rows: "M2.5 3.5h11M2.5 8h11M2.5 12.5h11",
      rowsDense: "M2.5 3h11M2.5 5.5h11M2.5 8h11M2.5 10.5h11M2.5 13h11",
      grid: "M2.5 2.5h4.5v4.5H2.5zM9 2.5h4.5v4.5H9zM2.5 9h4.5v4.5H2.5zM9 9h4.5v4.5H9z",
      download: "M8 2.5v8m0 0l3-3m-3 3l-3-3M2.5 13.5h11",
      close: "M4 4l8 8M12 4l-8 8",
      doc: "M4 2.5h5l3 3v8H4zM9 2.5v3h3",
      user: "M8 8a2.5 2.5 0 100-5 2.5 2.5 0 000 5zm-4.5 5.5a4.5 4.5 0 019 0",
      sidebar: "M2.5 3.5h11v9h-11zM6 3.5v9",
      panel: "M2.5 3.5h11v9h-11zM10 3.5v9",
      chevron: "M6 3l4 5-4 5",
      eye: "M1.5 8s2.4-4 6.5-4 6.5 4 6.5 4-2.4 4-6.5 4S1.5 8 1.5 8zM8 10a2 2 0 100-4 2 2 0 000 4z",
      command: "M6 2.5a1.5 1.5 0 11-1.5 1.5v8a1.5 1.5 0 11-1.5-1.5h10a1.5 1.5 0 11-1.5 1.5v-8A1.5 1.5 0 0112 4a1.5 1.5 0 111.5-1.5",
      layers: "M8 2l6 3-6 3-6-3 6-3zM2 8l6 3 6-3M2 11l6 3 6-3",
      alert: "M8 2.5l6 11H2l6-11zM8 6.5v3M8 11.5v.5",
      offline: "M2 2l12 12M5 8a4 4 0 015.5-.5M2.5 5.5A8 8 0 0113 6M8 12v.5",
    };
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("width", size);
    svg.setAttribute("height", size);
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "1.3");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("stroke-linejoin", "round");
    svg.setAttribute("aria-hidden", "true");
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", paths[name] || paths.files);
    svg.append(path);
    return svg;
  }

  function iconButton(name, label, onclick, extra = {}) {
    return el(
      "button",
      {
        class: "btn ghost icon-only",
        title: label,
        "aria-label": label,
        ...extra,
        onclick,
      },
      [svgIcon(name, extra.iconSize || 14)],
    );
  }

  // ---------- 主题与密度 ----------

  const THEMES = ["auto", "light", "dark", "hc"];

  function resolveTheme(pref) {
    if (pref === "light" || pref === "dark" || pref === "hc") return pref;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    return mq.matches ? "dark" : "light";
  }

  function applyTheme() {
    document.documentElement.dataset.theme = resolveTheme(state.themePref);
  }

  function applyDensity() {
    document.documentElement.dataset.density = state.density;
  }

  function applyRail() {
    document.body.classList.toggle("rail-expanded", state.railExpanded);
  }

  function applyLang() {
    document.documentElement.lang = lang;
    document.documentElement.dir = lang === "ar" || lang === "he" ? "rtl" : "ltr";
  }

  // ---------- API ----------

  class APIError extends Error {
    constructor(status, code, message) {
      super(message || code || `HTTP ${status}`);
      this.status = status;
      this.code = code;
    }
  }

  async function api(method, path, body) {
    if (!state.online) {
      throw new APIError(0, "offline", t("state.offlineTitle"));
    }

    const opts = { method, headers: {}, credentials: "same-origin" };
    if (body instanceof FormData) {
      opts.body = body;
    } else if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }

    const resp = await fetch("/api" + path, opts);

    if (resp.status === 401) throw new APIError(401, "unauthenticated", t("state.deniedTitle"));
    if (resp.status === 204) return null;

    const text = await resp.text();
    let payload = null;
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = { message: text };
      }
    }
    if (!resp.ok) {
      throw new APIError(resp.status, payload && payload.error, payload && payload.message);
    }
    return payload;
  }

  // ---------- 反馈 ----------

  let toastTimer = null;

  // announce 把消息送入 aria-live 区域。视觉提示与读屏提示必须同时发出：只做 toast 等于
  // 对读屏用户静默失败。
  function announce(message) {
    const live = $("#live");
    if (!live) return;
    live.textContent = "";
    // 同一句话连续两次需要重新插入，否则部分读屏不播报。
    setTimeout(() => {
      live.textContent = message;
    }, 20);
  }

  function toast(message, kind = "info", action) {
    const node = $("#toast");
    clear(node);
    node.append(el("span", { text: message }));
    if (action) {
      node.append(
        el("button", {
          text: action.label,
          onclick: () => {
            node.className = "";
            action.run();
          },
        }),
      );
    }
    node.className = "show" + (kind === "error" ? " error" : "");
    announce(message);
    clearTimeout(toastTimer);
    toastTimer = setTimeout(
      () => {
        node.className = "";
      },
      kind === "error" ? 6000 : 3200,
    );
  }

  function fail(err) {
    if (err instanceof APIError && err.status === 401) {
      location.href = "/api/auth/login";
      return;
    }
    toast(err.message || String(err), "error");
  }

  // ---------- 格式化 ----------

  const sizeFmt = new Intl.NumberFormat(lang, { maximumFractionDigits: 1 });

  function humanSize(bytes) {
    if (!bytes) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
    const value = bytes / Math.pow(1024, i);
    return `${i === 0 ? Math.round(value) : sizeFmt.format(value)} ${units[i]}`;
  }

  const relFmt = new Intl.RelativeTimeFormat(lang, { numeric: "auto" });

  function humanTime(iso) {
    if (!iso) return "";
    const seconds = (Date.now() - new Date(iso).getTime()) / 1000;
    if (seconds < 60) return relFmt.format(0, "second");
    if (seconds < 3600) return relFmt.format(-Math.floor(seconds / 60), "minute");
    if (seconds < 86400) return relFmt.format(-Math.floor(seconds / 3600), "hour");
    if (seconds < 86400 * 7) return relFmt.format(-Math.floor(seconds / 86400), "day");
    return new Date(iso).toLocaleDateString(lang, { month: "short", day: "numeric" });
  }

  const fullTimeFmt = new Intl.DateTimeFormat(lang, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });

  function fullTime(iso) {
    return iso ? fullTimeFmt.format(new Date(iso)) : "";
  }

  const clockFmt = new Intl.DateTimeFormat(lang, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });

  function clockOf(iso) {
    return clockFmt.format(new Date(iso));
  }

  const dayFmt = new Intl.DateTimeFormat(lang, { month: "short", day: "numeric" });

  function dayOf(iso) {
    return dayFmt.format(new Date(iso));
  }

  function joinPath(dir, name) {
    return dir ? `${dir}/${name}` : name;
  }

  function extensionOf(name) {
    const idx = name.lastIndexOf(".");
    if (idx <= 0 || idx === name.length - 1) return "";
    return name.slice(idx + 1);
  }

  function typeClass(entry) {
    if (entry.kind === "dir") return "dir";
    switch (entry.preview) {
      case "image": return "img";
      case "video": return "vid";
      case "audio": return "aud";
      case "pdf": return "doc";
      case "text": return "txt";
    }
    return "";
  }

  function badgeGlyph(entry) {
    if (entry.kind === "dir") return "";
    switch (entry.preview) {
      case "image": return "IMG";
      case "video": return "VID";
      case "audio": return "AUD";
      case "pdf": return "PDF";
      case "text": return "TXT";
    }
    return (extensionOf(entry.name) || "FILE").slice(0, 4);
  }

  function kindLabel(entry) {
    if (entry.kind === "dir") return t("kind.folder");
    if (entry.preview) return t(`kind.${entry.preview}`);
    const ext = extensionOf(entry.name);
    return ext ? ext : t("kind.file");
  }

  function isImage(entry) {
    return entry.kind !== "dir" && entry.preview === "image";
  }

  function filePath(entry) {
    return entry.path || joinPath(state.path, entry.name);
  }

  function previewURL(entry) {
    return `/api/files/download?inline=1&path=${encodeURIComponent(filePath(entry))}`;
  }

  function downloadURL(entry) {
    return `/api/files/download?path=${encodeURIComponent(filePath(entry))}`;
  }

  function thumbnailURL(entry) {
    return `/api/files/thumbnail?path=${encodeURIComponent(filePath(entry))}`;
  }

  // ---------- 选中 ----------

  function visibleEntries() {
    const q = state.filter.trim().toLowerCase();
    if (!q) return state.entries;
    return state.entries.filter((e) => e.name.toLowerCase().includes(q));
  }

  function selectedEntries() {
    return state.entries.filter((e) => state.selected.has(filePath(e)));
  }

  function selectOnly(entry) {
    state.selected.clear();
    if (entry) {
      const p = filePath(entry);
      state.selected.add(p);
      state.anchor = p;
    } else {
      state.anchor = null;
    }
  }

  function toggleSelect(entry) {
    const p = filePath(entry);
    if (state.selected.has(p)) state.selected.delete(p);
    else state.selected.add(p);
    state.anchor = p;
  }

  function extendSelection(entry) {
    const list = visibleEntries();
    const to = list.findIndex((e) => filePath(e) === filePath(entry));
    const from = list.findIndex((e) => filePath(e) === state.anchor);
    if (to < 0 || from < 0) {
      selectOnly(entry);
      return;
    }
    const [lo, hi] = from <= to ? [from, to] : [to, from];
    state.selected.clear();
    for (let i = lo; i <= hi; i++) state.selected.add(filePath(list[i]));
  }

  function clearSelection() {
    state.selected.clear();
    state.anchor = null;
    refresh();
  }

  function inspectedEntry() {
    if (state.selected.size !== 1) return null;
    const only = [...state.selected][0];
    return state.entries.find((e) => filePath(e) === only) || null;
  }

  // inspectorVisible 决定第三列是否存在。
  //
  // 这是"自适应"的核心：面板只在有对象时占位。关闭时（用户手动收起）也不占位。
  function inspectorVisible() {
    if (state.inspectorClosed) return false;
    if (state.view !== "files") return false;
    return inspectedEntry() !== null;
  }

  function applyInspector() {
    document.body.classList.toggle("has-inspector", inspectorVisible());
  }

  // ---------- 视图建议 ----------

  // suggestView 依据目录内容建议视图与密度，并给出理由。
  //
  // 建议而非强制：用户一旦手动切换就钉住，之后不再自动改变。没有"理由"的自动切换会让人
  // 觉得界面在自作主张。
  function suggestView(entries) {
    const files = entries.filter((e) => e.kind !== "dir");
    if (files.length === 0) return null;

    const images = files.filter((e) => e.preview === "image").length;
    const texts = files.filter((e) => e.preview === "text").length;

    if (files.length >= 6 && images / files.length >= 0.6) {
      return { viewMode: "gallery", density: "comfortable", reason: "hint.gallery", n: images };
    }
    if (entries.length > 200) {
      return { viewMode: "list", density: "compact", reason: "hint.compact", n: entries.length };
    }
    if (files.length >= 4 && texts / files.length >= 0.6) {
      return { viewMode: "list", density: "comfortable", reason: "hint.text", n: texts };
    }
    return null;
  }

  function applySuggestion() {
    const suggestion = suggestView(state.entries);
    state.suggestion = suggestion;
    if (state.viewPinned || !suggestion) return;
    state.viewMode = suggestion.viewMode;
    state.density = suggestion.density;
    applyDensity();
  }

  // ---------- 导航栏 ----------

  const NAV = [
    { key: "files", icon: "files" },
    { key: "shares", icon: "shares" },
    { key: "trash", icon: "trash" },
    { key: "activity", icon: "pulse" },
    { key: "access", icon: "key" },
  ];

  function renderRail() {
    const rail = $("#rail");
    clear(rail);

    rail.append(
      el("div", { class: "rail-head" }, [
        el("span", { class: "mark" }),
        el("span", { class: "word", text: t("app.name") }),
      ]),
    );

    const nav = el("nav", { class: "rail-nav", "aria-label": t("app.name") });
    for (const item of NAV) {
      nav.append(
        el(
          "button",
          {
            "aria-current": state.view === item.key ? "page" : null,
            title: t(`nav.${item.key}`),
            onclick: () => switchTo(item.key),
          },
          [
            el("span", { class: "icon" }, [svgIcon(item.icon)]),
            el("span", { class: "label", text: t(`nav.${item.key}`) }),
          ],
        ),
      );
    }
    rail.append(nav);

    rail.append(
      el("div", { class: "rail-foot" }, [
        el("span", { class: "avatar", "aria-hidden": "true", text: (state.me?.username || "?").slice(0, 1) }),
        el("span", { class: "who", text: state.me?.username || "" }),
        iconButton(state.railExpanded ? "sidebar" : "panel", state.railExpanded ? t("nav.collapse") : t("nav.expand"), () => {
          state.railExpanded = !state.railExpanded;
          safeSet("wc.rail", state.railExpanded ? "expanded" : "collapsed");
          applyRail();
          renderRail();
        }),
        iconButton("user", t("nav.signout"), async () => {
          try {
            await api("POST", "/auth/logout");
          } catch {
            /* cookie 无论如何都会被清 */
          }
          location.href = "/api/auth/login";
        }),
      ]),
    );
  }

  // ---------- 内容头部 ----------

  function renderHead() {
    const head = $("#head");
    clear(head);

    head.append(renderCrumbs());

    if (state.view === "files") {
      const fileInput = el("input", {
        type: "file",
        multiple: true,
        class: "hidden",
        onchange: (e) => uploadFiles(e.target.files),
      });

      head.append(
        el("button", { class: "btn primary", onclick: () => fileInput.click() }, [
          svgIcon("upload", 13),
          el("span", { text: t("head.upload") }),
        ]),
        iconButton("folderPlus", t("head.newFolder"), () => promptNewFolder()),
        fileInput,
      );
    }

    head.append(el("span", { class: "spacer" }));

    if (state.view === "files") {
      const search = el("input", {
        type: "search",
        id: "filter",
        placeholder: t("head.filter"),
        value: state.filter,
        "aria-label": t("head.filter"),
        oninput: (e) => {
          state.filter = e.target.value;
          renderBody();
          renderStatus();
        },
      });
      head.append(
        el("div", { class: "search" }, [
          search,
          el("span", { class: "icon" }, [svgIcon("search", 13)]),
        ]),
      );

      // 视图建议 chip：既说明理由，也是切换控件。
      if (state.suggestion || state.viewPinned) {
        const label = state.viewPinned
          ? t("hint.pinned", { view: t(state.viewMode === "gallery" ? "head.gallery" : "head.list") })
          : t(state.suggestion.reason, { n: state.suggestion.n });
        head.append(
          el("button", {
            class: "view-hint" + (state.viewPinned ? " pinned" : ""),
            title: t("head.view"),
            onclick: () => {
              state.viewPinned = !state.viewPinned;
              if (!state.viewPinned) applySuggestion();
              refresh();
            },
          }, [el("span", { text: label })]),
        );
      }

      head.append(
        el("div", { class: "seg", role: "group", "aria-label": t("head.view") }, [
          el("button", {
            "aria-pressed": state.viewMode === "list" ? "true" : "false",
            title: t("head.list"),
            "aria-label": t("head.list"),
            onclick: () => setViewMode("list"),
          }, [svgIcon("list", 13)]),
          el("button", {
            "aria-pressed": state.viewMode === "gallery" ? "true" : "false",
            title: t("head.gallery"),
            "aria-label": t("head.gallery"),
            onclick: () => setViewMode("gallery"),
          }, [svgIcon("grid", 13)]),
        ]),
      );

      head.append(
        el("div", { class: "seg", role: "group", "aria-label": t("head.density") }, [
          el("button", {
            "aria-pressed": state.density === "comfortable" ? "true" : "false",
            title: t("head.comfortable"),
            "aria-label": t("head.comfortable"),
            onclick: () => setDensity("comfortable"),
          }, [svgIcon("rows", 13)]),
          el("button", {
            "aria-pressed": state.density === "compact" ? "true" : "false",
            title: t("head.compact"),
            "aria-label": t("head.compact"),
            onclick: () => setDensity("compact"),
          }, [svgIcon("rowsDense", 13)]),
        ]),
      );
    }

    // 主题用原生 select：可键盘操作、有平台语义、读屏直接可读，不需要自绘菜单。
    const themeSelect = el("select", {
      "aria-label": t("head.theme"),
      title: t("head.theme"),
      class: "pick",
      onchange: (e) => {
        state.themePref = e.target.value;
        safeSet("wc.theme", state.themePref);
        applyTheme();
      },
    });
    for (const [value, label] of [
      ["auto", t("head.themeAuto")],
      ["light", t("head.themeLight")],
      ["dark", t("head.themeDark")],
      ["hc", t("head.themeHighContrast")],
    ]) {
      const opt = el("option", { value, text: label });
      if (value === state.themePref) opt.selected = true;
      themeSelect.append(opt);
    }
    head.append(themeSelect);

    head.append(iconButton("command", t("head.palette"), () => openPalette(), { iconSize: 15 }));

    if (state.view === "files") {
      head.append(
        iconButton("panel", t("head.details"), () => {
          state.inspectorClosed = !state.inspectorClosed;
          refresh();
        }, { "aria-pressed": state.inspectorClosed ? "false" : "true" }),
      );
    }
  }

  function renderCrumbs() {
    const box = el("div", { class: "crumbs" });

    const titles = { shares: "nav.shares", trash: "nav.trash", audit: "nav.activity", access: "nav.access" };
    if (titles[state.view]) {
      box.append(el("button", { class: "here", text: t(titles[state.view]) }));
      return box;
    }

    box.append(
      el("button", {
        class: state.path ? "" : "here",
        text: t("head.home"),
        onclick: () => navigate(""),
      }),
    );

    if (!state.path) return box;

    let acc = "";
    const segments = state.path.split("/");
    segments.forEach((seg, index) => {
      acc = acc ? `${acc}/${seg}` : seg;
      const target = acc;
      box.append(el("span", { class: "sep", text: "/" }));
      box.append(
        el("button", {
          class: index === segments.length - 1 ? "here" : "",
          text: seg,
          title: seg,
          onclick: () => navigate(target),
        }),
      );
    });
    return box;
  }

  function setViewMode(mode) {
    state.viewMode = mode;
    state.viewPinned = true;
    refresh();
  }

  function setDensity(density) {
    state.density = density;
    state.viewPinned = true;
    safeSet("wc.density", density);
    applyDensity();
    refresh();
  }

  // ---------- 主体 ----------

  function refresh() {
    applyInspector();
    renderRail();
    renderHead();
    renderBody();
    renderInspector();
    renderStatus();
  }

  function renderBody() {
    const body = $("#body");
    clear(body);

    const progress = el("progress", { class: "progress hidden", id: "upload-progress", max: "100", value: "0" });
    body.append(progress);

    if (!state.online) {
      body.append(renderBanner("offline", t("banner.offline")));
    }

    const wrap = el("div", { class: "content-body" });
    body.append(wrap);

    switch (state.view) {
      case "files":
        wrap.append(renderFiles());
        break;
      case "shares":
        wrap.append(renderShares());
        break;
      case "trash":
        wrap.append(renderTrash());
        break;
      case "audit":
        wrap.append(renderAudit());
        break;
      case "access":
        wrap.append(renderAccess());
        break;
    }

    // Honour a pending focus request now that the listbox exists again.
    if (refocusList) {
      refocusList = false;
      const lb = document.querySelector('.filelist[role="listbox"], .gallery[role="listbox"]');
      if (lb) lb.focus();
    }

    // 多选条
    const selected = selectedEntries();
    if (state.view === "files" && selected.length > 1) {
      const total = selected.reduce((sum, e) => sum + (e.size || 0), 0);
      wrap.append(
        el("div", { class: "selection-bar", role: "toolbar", "aria-label": t("count.selected_other", { n: selected.length }) }, [
          el("span", { class: "count", text: t("count.selected_other", { n: selected.length }) }),
          el("span", { class: "mono dim", text: humanSize(total) }),
          el("button", { text: t("action.download"), onclick: () => selected.forEach(downloadFile) }),
          el("button", { class: "strong", text: t("action.delete"), onclick: () => confirmDelete(null) }),
          el("button", { text: t("action.clear"), onclick: () => clearSelection() }),
        ]),
      );
    }
  }

  function renderBanner(kind, message) {
    return el("div", { class: `banner ${kind}`, role: "status" }, [
      svgIcon(kind === "offline" ? "offline" : "alert", 14),
      el("span", { text: message }),
      el("span", { class: "spacer" }),
    ]);
  }

  function renderFiles() {
    const entries = visibleEntries();

    if (state.entries.length === 0) {
      return renderState("files", t("state.emptyTitle"), t("state.emptyHint"), [
        { label: t("head.upload"), primary: true, run: () => $("#head input[type=file]")?.click() },
      ]);
    }

    if (entries.length === 0) {
      return renderState("search", t("state.noMatchTitle"), t("state.noMatchHint"), [
        {
          label: t("state.clearFilter"),
          run: () => {
            state.filter = "";
            refresh();
          },
        },
      ]);
    }

    return state.viewMode === "gallery" ? renderGallery(entries) : renderList(entries);
  }

  function renderState(icon, title, hint, actions = [], code) {
    return el("div", { class: "state" }, [
      el("div", { class: "glyph" }, [svgIcon(icon, 26)]),
      el("div", { class: "title", text: title }),
      el("div", { class: "hint", text: hint }),
      actions.length
        ? el("div", { class: "actions" }, actions.map((a) =>
            el("button", {
              class: "btn" + (a.primary ? " primary" : ""),
              text: a.label,
              onclick: a.run,
            }),
          ))
        : null,
      code ? el("div", { class: "code", text: code }) : null,
    ]);
  }

  // renderList 用 role="listbox" + aria-activedescendant。
  //
  // 这样整个列表是一个 Tab 停留点，方向键在内部移动——比让每行都可 Tab 更符合文件管理器的
  // 预期，也让读屏不必逐行 Tab 穿过几百个文件。
  function renderList(entries) {
    const wrap = el("div", { class: "scroll" });

    wrap.append(
      el("div", { class: "filelist-head", "aria-hidden": "true" }, [
        el("span", { text: t("list.name") }),
        el("span", { class: "end", text: t("list.size") }),
        el("span", { class: "when", text: t("list.modified") }),
        el("span", { class: "end", text: t("list.marks") }),
      ]),
    );

    const focusPath = state.anchor;
    const list = el("div", {
      class: "filelist",
      role: "listbox",
      "aria-label": t("nav.files"),
      tabindex: "0",
      "aria-activedescendant": focusPath ? rowID(focusPath) : null,
      onkeydown: onListKey,
    });

    entries.forEach((entry, index) => {
      list.append(renderRow(entry, index));
    });

    wrap.append(list);
    return wrap;
  }

  function rowID(path) {
    return "r-" + path.replace(/[^a-zA-Z0-9_-]/g, "_");
  }

  function renderRow(entry, index) {
    const path = filePath(entry);
    const selected = state.selected.has(path);
    const marks = [];

    // 内联的分享/版本标记：不动手就能看出这个对象是否被共享、是否留有历史。
    if (entry.share_count) {
      marks.push(el("span", { class: "m", title: t("count.links_other", { n: entry.share_count }) }, [
        svgIcon("shares", 11),
        el("span", { text: String(entry.share_count) }),
      ]));
    }
    if (entry.has_versions) {
      marks.push(el("span", { class: "m", title: t("insp.versions") }, [svgIcon("layers", 11)]));
    }

    return el("div", {
      class: "row",
      id: rowID(path),
      role: "option",
      "aria-selected": selected ? "true" : "false",
      "aria-posinset": String(index + 1),
      "aria-setsize": String(visibleEntries().length),
      dataset: { path },
      onclick: (e) => onRowClick(e, entry),
      ondblclick: () => openEntry(entry),
      oncontextmenu: (e) => {
        e.preventDefault();
        if (!state.selected.has(path)) selectOnly(entry);
        refresh();
        openShareDialog(entry);
      },
    }, [
      el("div", { class: "name" }, [
        fileGlyph(entry),
        el("span", { class: "label" + (entry.kind === "dir" ? " dir" : ""), text: entry.name }),
      ]),
      el("div", { class: "size", text: entry.kind === "dir" ? "—" : humanSize(entry.size) }),
      el("div", { class: "when", text: humanTime(entry.modified_at) }),
      el("div", { class: "marks" }, marks),
    ]);
  }

  function fileGlyph(entry) {
    if (isImage(entry)) {
      return el("img", {
        class: "thumb",
        src: thumbnailURL(entry),
        alt: "",
        loading: "lazy",
        onerror: (e) => {
          e.target.replaceWith(el("span", { class: `ftype ${typeClass(entry)}` }, [badgeGlyph(entry)]));
        },
      });
    }
    if (entry.kind === "dir") {
      return el("span", { class: "ftype dir", "aria-hidden": "true" }, [svgIcon("files", 12)]);
    }
    return el("span", { class: `ftype ${typeClass(entry)}`, "aria-hidden": "true" }, [badgeGlyph(entry)]);
  }

  function renderGallery(entries) {
    const wrap = el("div", { class: "scroll" });
    const grid = el("div", {
      class: "gallery",
      role: "listbox",
      "aria-label": t("nav.files"),
      tabindex: "0",
      onkeydown: onListKey,
    });

    for (const entry of entries) {
      const path = filePath(entry);
      const selected = state.selected.has(path);

      let preview;
      if (isImage(entry)) {
        preview = el("div", { class: "preview" }, [
          el("img", {
            src: thumbnailURL(entry),
            alt: "",
            loading: "lazy",
            onerror: (e) => {
              e.target.replaceWith(el("span", { class: "glyph" }, [svgIcon("doc", 24)]));
            },
          }),
        ]);
      } else {
        preview = el("div", { class: "preview" + (entry.kind === "dir" ? " dir" : "") }, [
          el("span", { class: "glyph" }, [svgIcon(entry.kind === "dir" ? "files" : "doc", 24)]),
        ]);
      }

      grid.append(
        el("div", {
          class: "tile",
          id: rowID(path),
          role: "option",
          "aria-selected": selected ? "true" : "false",
          title: entry.name,
          dataset: { path },
          onclick: (e) => onRowClick(e, entry),
          ondblclick: () => openEntry(entry),
        }, [
          preview,
          el("div", { class: "meta" }, [
            el("div", { class: "n", text: entry.name }),
            el("div", { class: "s", text: entry.kind === "dir" ? t("kind.folder") : humanSize(entry.size) }),
          ]),
        ]),
      );
    }

    wrap.append(grid);
    return wrap;
  }

  function onListKey(e) {
    const list = visibleEntries();
    if (list.length === 0) return;

    const current = inspectedEntry();
    const index = current ? list.findIndex((x) => filePath(x) === filePath(current)) : -1;

    const focus = (i) => {
      if (i < 0 || i >= list.length) return;
      selectOnly(list[i]);
      // The render rebuilds the listbox, so ask it to take focus back afterwards; the new
      // element carries aria-activedescendant from state.anchor.
      requestListFocus();
      refresh();
      loadDetailIfNeeded();
      const node = document.getElementById(rowID(filePath(list[i])));
      if (node && node.scrollIntoView) node.scrollIntoView({ block: "nearest" });
    };

    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        focus(index < 0 ? 0 : Math.min(index + 1, list.length - 1));
        break;
      case "ArrowUp":
        e.preventDefault();
        focus(index < 0 ? list.length - 1 : Math.max(index - 1, 0));
        break;
      case "Home":
        e.preventDefault();
        focus(0);
        break;
      case "End":
        e.preventDefault();
        focus(list.length - 1);
        break;
      case "PageDown":
        e.preventDefault();
        focus(Math.min((index < 0 ? 0 : index) + 10, list.length - 1));
        break;
      case "PageUp":
        e.preventDefault();
        focus(Math.max((index < 0 ? 0 : index) - 10, 0));
        break;
      case "Enter":
        e.preventDefault();
        if (current) openEntry(current);
        break;
      case "a":
        if (e.metaKey || e.ctrlKey) {
          e.preventDefault();
          state.selected.clear();
          for (const entry of list) state.selected.add(filePath(entry));
          refresh();
        }
        break;
    }
  }

  function onRowClick(e, entry) {
    if (e.shiftKey) extendSelection(entry);
    else if (e.metaKey || e.ctrlKey) toggleSelect(entry);
    else selectOnly(entry);
    // Rows are not focusable; the listbox is. Move focus there so the arrow keys work
    // immediately after a click, which is what people expect from a file manager.
    requestListFocus();
    refresh();
    loadDetailIfNeeded();
  }

  function openEntry(entry) {
    if (entry.kind === "dir") {
      navigate(filePath(entry));
      return;
    }
    if (entry.preview) {
      openPreview(entry);
      return;
    }
    downloadFile(entry);
  }

  function navigate(path) {
    state.path = path;
    state.filter = "";
    state.selected.clear();
    state.anchor = null;
    state.viewPinned = false;
    state.detail = { path: null, versions: null, shares: null, loading: false };
    loadFiles();
  }

  async function loadFiles() {
    renderSkeleton();
    try {
      const data = await api("GET", `/files/?path=${encodeURIComponent(state.path)}`);
      state.entries = data.children || [];
      applySuggestion();
      refresh();
    } catch (err) {
      renderError(err);
    }
  }

  // renderSkeleton 先画骨架再请求：空屏再跳变比等一会儿更让人怀疑"是不是坏了"。
  function renderSkeleton() {
    const body = $("#body");
    clear(body);
    const wrap = el("div", { class: "content-body" });
    const scroll = el("div", { class: "scroll" });
    for (let i = 0; i < 8; i++) {
      scroll.append(
        el("div", { class: "skeleton-row", "aria-hidden": "true" }, [
          el("div", { class: "bar" }),
          el("div", { class: "bar" }),
          el("div", { class: "bar" }),
        ]),
      );
    }
    wrap.append(scroll);
    body.append(wrap);
  }

  function renderError(err) {
    const body = $("#body");
    clear(body);
    const wrap = el("div", { class: "content-body" });

    // 403/404 同形：不提供"此对象是否存在"的探测口。
    if (err instanceof APIError && (err.status === 403 || err.status === 404)) {
      wrap.append(
        renderState("alert", t("state.deniedTitle"), t("state.deniedHint"), [
          { label: t("head.home"), run: () => navigate("") },
        ]),
      );
    } else {
      wrap.append(
        renderState("alert", t("state.errorTitle"), err.message || "", [
          { label: t("state.retry"), primary: true, run: () => loadFiles() },
        ], err.code || ""),
      );
    }
    body.append(wrap);
    announce(t("state.errorTitle"));
  }

  function downloadFile(entry) {
    const link = el("a", { href: downloadURL(entry), download: entry.name });
    document.body.append(link);
    link.click();
    link.remove();
  }

  // ---------- 详情面板 ----------

  function renderInspector() {
    const panel = $("#inspector");
    clear(panel);

    if (!inspectorVisible()) {
      clear(panel);
      return;
    }

    const entry = inspectedEntry();

    panel.append(
      el("div", { class: "inspector-head" }, [
        el("span", { class: "title", text: t("insp.details") }),
        iconButton("close", t("insp.close"), () => clearSelection(), { iconSize: 13 }),
      ]),
    );

    const body = el("div", { class: "inspector-body" });
    body.append(renderStage(entry));
    body.append(
      el("div", { class: "inspector-file" }, [
        el("div", { class: "name", text: entry.name }),
        el("div", { class: "path", text: "/" + filePath(entry) }),
      ]),
    );
    body.append(renderProps(entry));
    body.append(renderActions(entry));

    if (entry.kind !== "dir") {
      body.append(renderVersions(entry));
      body.append(renderShareSection(entry));
    }

    panel.append(body);
  }

  function renderStage(entry) {
    const stage = el("div", { class: "stage" });

    if (isImage(entry)) {
      stage.append(
        el("img", {
          src: previewURL(entry),
          alt: "",
          onerror: () => {
            clear(stage);
            stage.append(el("span", { class: "glyph" }, [svgIcon("doc", 26)]));
            stage.append(el("div", { class: "note", text: t("insp.stageError") }));
          },
        }),
      );
    } else if (entry.kind === "dir") {
      stage.append(el("span", { class: "glyph" }, [svgIcon("files", 26)]));
    } else if (entry.preview === "text") {
      stage.classList.add("text-stage");
      const pre = el("pre", { text: "" });
      stage.append(pre);
      fetchTextHead(entry, pre);
    } else if (entry.preview) {
      stage.append(el("span", { class: "glyph" }, [svgIcon("doc", 26)]));
    } else {
      stage.append(el("div", { class: "note", text: t("insp.stageNone") }));
    }

    if (entry.kind !== "dir" && entry.preview) {
      stage.append(
        el("button", { class: "open-hint", text: t("insp.openPreview"), onclick: () => openPreview(entry) }),
      );
    }
    return stage;
  }

  // fetchTextHead 只取前 9 行。
  //
  // 目的是"认出这是哪个文件"，不是阅读；5MB 的日志不该为了 176px 的面板被拉过来。
  async function fetchTextHead(entry, pre) {
    try {
      const resp = await fetch(previewURL(entry), { credentials: "same-origin" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const text = await resp.text();
      pre.textContent = text.split("\n").slice(0, 9).join("\n");
    } catch {
      pre.textContent = t("insp.stageError");
    }
  }

  function renderProps(entry) {
    const dl = el("dl");
    const add = (key, value) => {
      dl.append(el("dt", { text: key }));
      dl.append(el("dd", { text: value }));
    };
    add(t("insp.kind"), kindLabel(entry));
    if (entry.kind !== "dir") add(t("insp.size"), humanSize(entry.size));
    add(t("insp.modified"), fullTime(entry.modified_at));
    const dlWrap = el("div", { class: "props" }, [dl]);
    return dlWrap;
  }

  function renderActions(entry) {
    const row = el("div", { class: "inspector-actions" });
    if (entry.kind !== "dir" && entry.preview) {
      row.append(el("button", { class: "btn small", onclick: () => openPreview(entry) }, [svgIcon("eye", 12), el("span", { text: t("insp.open") })]));
    }
    if (entry.kind !== "dir") {
      row.append(el("button", { class: "btn small", onclick: () => downloadFile(entry) }, [svgIcon("download", 12), el("span", { text: t("insp.download") })]));
    }
    row.append(el("button", { class: "btn small", onclick: () => openShareDialog(entry) }, [svgIcon("shares", 12), el("span", { text: t("insp.share") })]));
    row.append(el("button", { class: "btn small", onclick: () => promptRename(entry) }, [el("span", { text: t("insp.rename") })]));
    row.append(el("button", { class: "btn small danger", onclick: () => confirmDelete(entry) }, [el("span", { text: t("insp.delete") })]));
    return row;
  }

  function renderVersions(entry) {
    const section = el("details", { class: "inspector-section" });
    const bodyWrap = el("div", { class: "section-body" });
    const loading = state.detail.path !== filePath(entry) || state.detail.versions == null;

    section.append(
      el("summary", {}, [
        el("span", { class: "chev" }, [svgIcon("chevron", 11)]),
        el("span", { class: "micro", text: t("insp.versions") }),
        el("span", { class: "count", text: loading ? "…" : String(state.detail.versions.length) }),
      ]),
    );

    if (loading) bodyWrap.append(el("div", { class: "note-empty", text: t("insp.loading") }));
    else if (state.detail.versions.length === 0) bodyWrap.append(el("div", { class: "note-empty", text: t("insp.noVersions") }));
    else {
      for (const v of state.detail.versions) {
        bodyWrap.append(
          el("div", { class: "version-row" }, [
            el("span", { class: "vno", text: `v${v.version}` }),
            el("span", { class: "vmeta", text: `${humanSize(v.size)} · ${humanTime(v.created_at)}` }),
            el("button", { class: "row-action act", text: t("insp.restore"), onclick: () => restoreVersion(entry, v) }),
          ]),
        );
      }
    }

    section.append(bodyWrap);
    return section;
  }

  function renderShareSection(entry) {
    const section = el("details", { class: "inspector-section" });
    const bodyWrap = el("div", { class: "section-body" });
    const path = filePath(entry);
    const loading = state.detail.path !== path || state.detail.shares == null;
    const shares = state.detail.shares || [];

    section.append(
      el("summary", {}, [
        el("span", { class: "chev" }, [svgIcon("chevron", 11)]),
        el("span", { class: "micro", text: t("insp.sharedBy") }),
        el("span", { class: "count", text: loading ? "…" : String(shares.length) }),
      ]),
    );

    if (loading) bodyWrap.append(el("div", { class: "note-empty", text: t("insp.loading") }));
    else if (shares.length === 0) bodyWrap.append(el("div", { class: "note-empty", text: t("insp.notShared") }));
    else {
      for (const s of shares) {
        const used =
          s.kind === "request"
            ? `${s.upload_count}${s.max_uploads ? "/" + s.max_uploads : ""}`
            : `${s.download_count}${s.max_downloads ? "/" + s.max_downloads : ""}`;
        bodyWrap.append(
          el("div", { class: "share-row" }, [
            el("span", { class: "pill" + (s.kind === "request" ? " accent" : ""), text: s.kind }),
            el("span", { class: "used", text: used }),
            // 不提供"复制原链接"：只存了 token 的哈希，URL 无法重建。给出永远不会成功的
            // 按钮比说明它为什么不在更糟。
            el("span", { class: "note-empty", text: t("insp.linkNotRecoverable") }),
            el("button", {
              class: "row-action danger",
              text: t("action.revoke"),
              onclick: async () => {
                try {
                  await api("DELETE", `/shares/${s.id}`);
                  toast(t("toast.shareRevoked"));
                  reloadDetail(entry);
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        );
      }
    }

    section.append(bodyWrap);
    return section;
  }

  async function loadDetailIfNeeded() {
    const entry = inspectedEntry();
    if (!entry || entry.kind === "dir") return;
    const path = filePath(entry);
    if (state.detail.path === path && state.detail.versions != null) return;

    state.detail = { path, versions: null, shares: null, loading: true };

    const [versions, shares] = await Promise.all([
      api("GET", `/files/versions?path=${encodeURIComponent(path)}`).catch(() => ({ versions: [] })),
      api("GET", "/shares/").catch(() => ({ items: [] })),
    ]);

    // 请求期间选中可能已经移走；丢弃过期结果，否则会显示另一个文件的版本。
    if (state.detail.path !== path) return;

    state.detail = {
      path,
      versions: versions.versions || [],
      shares: (shares.items || []).filter((s) => s.node_id === entry.id),
      loading: false,
    };
    renderInspector();
  }

  function reloadDetail(entry) {
    state.detail = { path: null, versions: null, shares: null, loading: false };
    renderInspector();
    loadDetailIfNeeded();
  }

  async function restoreVersion(entry, version) {
    try {
      await api("POST", "/files/restore-version", { path: filePath(entry), version: version.version });
      toast(t("toast.restoredVersion", { version: version.version }));
      await loadFiles();
      reloadDetail(entry);
    } catch (err) {
      fail(err);
    }
  }

  // ---------- 状态条 ----------

  function renderStatus() {
    const bar = $("#status");
    clear(bar);

    const dot = el("span", { class: "dot" + (state.online ? "" : " off") });
    bar.append(dot);
    bar.append(el("span", { text: state.online ? t("status.online") : t("status.offline") }));

    if (state.me && state.me.quota_bytes) {
      bar.append(el("span", { text: "·" }));
      bar.append(
        el("span", {
          text: t("status.storage", {
            used: humanSize(state.me.used_bytes || 0),
            quota: humanSize(state.me.quota_bytes),
          }),
        }),
      );
    } else if (state.me && state.me.used_bytes) {
      bar.append(el("span", { text: "·" }));
      bar.append(el("span", { text: t("status.storageFree", { used: humanSize(state.me.used_bytes) }) }));
    }

    if (state.me && state.me.storage && state.me.storage.layout) {
      bar.append(el("span", { text: "·" }));
      bar.append(el("span", { text: safeText(state.me.storage.layout) }));
    }

    bar.append(el("span", { class: "spacer" }));

    if (state.view === "files") {
      const total = state.entries.length;
      const shown = visibleEntries().length;
      bar.append(
        el("span", {
          text: shown !== total ? t("count.shown", { shown, total }) : t("count.items", { n: total }),
        }),
      );
    }
  }

  // safeText 把后端枚举值限制为短串。
  //
  // 状态条直接显示后端的 layout 值，但那是运维可配的字符串。这里只允许已知枚举，其余一律
  // 不显示，避免把配置内容当文案渲染进界面。
  function safeText(value) {
    const allowed = ["blob", "tree", "local", "s3"];
    return allowed.includes(value) ? value : "";
  }

  // ---------- 命令面板 ----------

  function commandList() {
    const cmds = [
      { id: "files", label: t("palette.goFiles"), run: () => switchTo("files") },
      { id: "shares", label: t("palette.goShares"), run: () => switchTo("shares") },
      { id: "trash", label: t("palette.goTrash"), run: () => switchTo("trash") },
      { id: "activity", label: t("palette.goActivity"), run: () => switchTo("activity") },
      { id: "access", label: t("palette.goAccess"), run: () => switchTo("access") },
      { id: "upload", label: t("palette.upload"), run: () => $("#head input[type=file]")?.click() },
      { id: "newfolder", label: t("palette.newFolder"), run: () => promptNewFolder() },
      { id: "theme", label: t("palette.toggleTheme"), run: () => cycleTheme() },
      { id: "density", label: t("palette.toggleDensity"), run: () => setDensity(state.density === "compact" ? "comfortable" : "compact") },
      { id: "rail", label: t("palette.toggleRail"), run: () => { state.railExpanded = !state.railExpanded; safeSet("wc.rail", state.railExpanded ? "expanded" : "collapsed"); applyRail(); refresh(); } },
      { id: "inspector", label: t("palette.toggleInspector"), run: () => { state.inspectorClosed = !state.inspectorClosed; refresh(); } },
      { id: "selectall", label: t("palette.selectAll"), run: () => { state.selected.clear(); for (const e of visibleEntries()) state.selected.add(filePath(e)); refresh(); } },
      { id: "clearsel", label: t("palette.clearSelection"), run: () => clearSelection() },
    ];
    return cmds;
  }

  function cycleTheme() {
    const idx = THEMES.indexOf(state.themePref);
    state.themePref = THEMES[(idx + 1) % THEMES.length];
    safeSet("wc.theme", state.themePref);
    applyTheme();
    refresh();
  }

  // fuzzyMatch 做子序列匹配：输入 "rdme" 命中 "README.txt"。命令面板的价值就在于不必输入全名。
  function fuzzyMatch(needle, haystack) {
    needle = needle.toLowerCase();
    haystack = haystack.toLowerCase();
    if (!needle) return true;
    let i = 0;
    for (const ch of haystack) {
      if (ch === needle[i]) i++;
      if (i === needle.length) return true;
    }
    return false;
  }

  function openPalette() {
    if (state.paletteOpen) return;
    state.paletteOpen = true;

    const input = el("input", { type: "text", placeholder: t("palette.placeholder"), "aria-label": t("palette.placeholder") });
    const listNode = el("ul", { role: "listbox", "aria-label": t("palette.placeholder") });
    const scrim = el("div", { class: "palette-scrim", onclick: () => closePalette() });
    const panel = el("div", {
      class: "palette",
      role: "dialog",
      "aria-modal": "true",
      "aria-label": t("palette.placeholder"),
    });

    let results = [];
    let active = 0;

    function rebuild() {
      const q = input.value.trim();
      const files = state.view === "files"
        ? visibleEntries()
            .slice(0, 40)
            .filter((e) => fuzzyMatch(q, e.name))
            .map((e) => ({ kind: t("palette.files"), label: e.name, run: () => openEntry(e) }))
        : [];
      const cmds = commandList()
        .filter((c) => fuzzyMatch(q, c.label))
        .map((c) => ({ kind: t("palette.commands"), label: c.label, run: c.run }));

      results = [...cmds, ...files];
      active = 0;

      clear(listNode);
      if (results.length === 0) {
        listNode.append(el("li", { class: "empty", text: t("palette.empty") }));
        return;
      }
      results.forEach((r, i) => {
        listNode.append(
          el("li", {
            role: "option",
            "aria-selected": i === active ? "true" : "false",
            onclick: () => {
              closePalette();
              r.run();
            },
            onmouseenter: () => {
              active = i;
              highlight();
            },
          }, [
            el("span", { class: "kind", text: r.kind }),
            el("span", { class: "label", text: r.label }),
          ]),
        );
      });
      highlight();
    }

    function highlight() {
      [...listNode.children].forEach((li, i) => {
        li.setAttribute("aria-selected", i === active ? "true" : "false");
        if (i === active && li.scrollIntoView) li.scrollIntoView({ block: "nearest" });
      });
    }

    input.addEventListener("input", rebuild);
    input.addEventListener("keydown", (e) => {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        active = Math.min(active + 1, results.length - 1);
        highlight();
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        active = Math.max(active - 1, 0);
        highlight();
      } else if (e.key === "Enter") {
        e.preventDefault();
        const chosen = results[active];
        closePalette();
        if (chosen) chosen.run();
      } else if (e.key === "Escape") {
        e.preventDefault();
        closePalette();
      }
    });

    panel.append(input, listNode,
      el("div", { class: "palette-foot" }, [
        el("span", { text: t("palette.hintNav") }),
        el("span", { text: t("palette.hintRun") }),
        el("span", { text: t("palette.hintClose") }),
      ]),
    );

    document.body.append(scrim, panel);
    rebuild();
    input.focus();
  }

  function closePalette() {
    state.paletteOpen = false;
    document.querySelector(".palette")?.remove();
    document.querySelector(".palette-scrim")?.remove();
  }

  // ---------- 预览 ----------

  let activeDialog = null;

  function showDialog(dialog) {
    if (activeDialog) activeDialog.close();
    document.body.append(dialog);
    activeDialog = dialog;
    dialog.addEventListener("close", () => {
      dialog.remove();
      if (activeDialog === dialog) activeDialog = null;
    });
    dialog.showModal();
  }

  function openPreview(entry) {
    const kind = entry.preview;
    const url = previewURL(entry);
    const body = el("div", { class: "preview-body" });

    switch (kind) {
      case "image":
        body.append(el("img", { src: url, alt: entry.name, onerror: () => { clear(body); body.append(unsupported(t("preview.imageFailed"))); } }));
        break;
      case "video":
        body.append(el("video", { src: url, controls: true, preload: "metadata", onerror: () => { clear(body); body.append(unsupported(t("preview.videoFailed"))); } }));
        break;
      case "audio":
        body.append(el("audio", { src: url, controls: true, preload: "metadata", onerror: () => { clear(body); body.append(unsupported(t("preview.audioFailed"))); } }));
        break;
      case "pdf":
        // 不给 iframe 加 sandbox：Chrome 会因此拒绝实例化 PDF 插件并显示破页图标。
        // 响应本身带 CSP sandbox，那才是生效的那一层。
        body.append(el("iframe", { src: url, title: entry.name }));
        break;
      case "text":
        body.append(el("pre", { text: t("preview.loading") }));
        loadTextPreview(url, body);
        break;
      default:
        body.append(unsupported(t("preview.none")));
    }

    const dialog = el("dialog", { class: "preview", "aria-label": entry.name }, [
      el("div", { class: "preview-head" }, [
        el("span", { class: "title", text: entry.name, title: entry.name }),
        el("span", { class: "sub", text: humanSize(entry.size) }),
        el("span", { class: "spacer" }),
        el("button", { class: "btn ghost", onclick: () => downloadFile(entry) }, [svgIcon("download", 13), el("span", { text: t("insp.download") })]),
        iconButton("close", t("action.cancel"), () => dialog.close(), { iconSize: 13 }),
      ]),
      body,
    ]);

    showDialog(dialog);
  }

  function unsupported(message) {
    return el("div", { class: "state" }, [
      el("div", { class: "title", text: t("preview.unavailable") }),
      el("div", { class: "hint", text: message }),
    ]);
  }

  async function loadTextPreview(url, container) {
    try {
      const resp = await fetch(url, { credentials: "same-origin" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const text = await resp.text();
      clear(container);
      container.append(el("pre", { text }));
    } catch (err) {
      clear(container);
      container.append(unsupported(t("preview.loadFailed", { msg: err.message })));
    }
  }

  // ---------- 上传 ----------

  let uploading = false;

  async function uploadFiles(fileList, targetPath) {
    if (!fileList || fileList.length === 0) return;
    if (uploading) {
      toast(t("toast.uploading"), "error");
      return;
    }
    if (!state.online) {
      toast(t("state.offlineTitle"), "error");
      return;
    }

    const dest = targetPath == null ? state.path : targetPath;
    uploading = true;

    const progress = $("#upload-progress");
    const bar = progress && progress.firstElementChild;
    const total = fileList.length;
    let done = 0;
    let failed = 0;
    const failures = [];

    if (progress) progress.classList.remove("hidden");

    for (const file of fileList) {
      const form = new FormData();
      form.append("path", dest);
      form.append("file", file);

      try {
        await api("POST", "/files/upload", form);
        done++;
      } catch (err) {
        failed++;
        failures.push(`${file.name}: ${err.message || ""}`);
      }
      if (bar) bar.value = Math.round(((done + failed) / total) * 100);
      renderStatusUpload(done + failed, total);
    }

    uploading = false;
    if (progress) {
      setTimeout(() => {
        progress.classList.add("hidden");
        if (bar) bar.value = 0;
      }, 350);
    }

    if (done > 0 || failed > 0) {
      if (failed === 0) toast(t("toast.uploaded", { n: done }));
      else toast(t("toast.uploadedSome", { done, failed }), "error");
    }

    if (dest === state.path) await loadFiles();
  }

  function renderStatusUpload(done, total) {
    const bar = $("#status");
    if (!bar) return;
    let node = bar.querySelector(".upload-state");
    if (!node) {
      node = el("span", { class: "upload-state" });
      bar.append(node);
    }
    node.textContent = "· " + t("status.uploading", { done, total });
  }

  // ---------- 对话框 ----------

  function formDialog({ title, body, fields = [], confirm, danger, onSubmit, extra }) {
    const form = el("form", { method: "dialog" });
    const values = {};

    form.append(
      el("div", { class: "dlg-head" }, [
        el("h2", { text: title }),
        body ? el("p", { text: body }) : null,
      ]),
    );

    const bodyWrap = el("div", { class: "dlg-body" });
    if (extra) bodyWrap.append(extra);

    for (const field of fields) {
      let input;
      if (field.type === "select") {
        input = el("select", { name: field.name });
        for (const opt of field.options) input.append(el("option", { value: opt.value, text: opt.label }));
        if (field.value) input.value = field.value;
      } else {
        input = el("input", {
          type: field.type,
          name: field.name,
          value: field.value == null ? "" : field.value,
          placeholder: field.placeholder || "",
          min: field.min,
          max: field.max,
        });
      }
      values[field.name] = input;
      bodyWrap.append(el("label", { class: "field" }, [el("span", { class: "micro", text: field.label }), input]));
    }
    form.append(bodyWrap);

    const submit = el("button", { class: danger ? "btn danger" : "btn primary", text: confirm, type: "submit" });
    const cancel = el("button", { class: "btn", text: t("action.cancel"), type: "button", onclick: () => dialog.close() });
    form.append(el("menu", {}, [cancel, submit]));

    const dialog = el("dialog", {}, [form]);

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const out = {};
      for (const [name, input] of Object.entries(values)) out[name] = input.value.trim();
      submit.disabled = true;
      try {
        await onSubmit(out);
        dialog.close();
      } catch (err) {
        fail(err);
        submit.disabled = false;
      }
    });

    showDialog(dialog);
    const first = fields.length > 0 ? values[fields[0].name] : null;
    if (first) {
      first.focus();
      if (first.select) first.select();
    }
    return dialog;
  }

  function promptNewFolder() {
    formDialog({
      title: t("dlg.newFolder"),
      fields: [{ name: "name", label: t("dlg.folderName"), type: "text" }],
      confirm: t("action.create"),
      onSubmit: async (v) => {
        if (!v.name) throw new Error(t("dlg.folderName"));
        await api("POST", "/files/mkdir", { path: joinPath(state.path, v.name) });
        toast(t("toast.folderCreated"));
        await loadFiles();
      },
    });
  }

  function promptRename(entry) {
    formDialog({
      title: t("dlg.rename"),
      body: entry.name,
      fields: [{ name: "name", label: t("dlg.newName"), type: "text", value: entry.name }],
      confirm: t("action.rename"),
      onSubmit: async (v) => {
        if (!v.name || v.name === entry.name) return;
        await api("POST", "/files/rename", { path: filePath(entry), name: v.name });
        toast(t("toast.renamed"));
        await loadFiles();
      },
    });
  }

  function confirmDelete(entry) {
    const targets = entry ? (state.selected.size > 1 ? selectedEntries() : [entry]) : selectedEntries();
    if (targets.length === 0) return;

    const one = targets.length === 1;
    formDialog({
      title: one ? t("dlg.deleteTitle", { name: targets[0].name }) : t("dlg.deleteManyTitle", { n: targets.length }),
      body: t("dlg.deleteBody"),
      fields: [],
      confirm: t("action.delete"),
      danger: true,
      onSubmit: async () => {
        const before = targets.map((t2) => filePath(t2));
        for (const target of targets) {
          await api("DELETE", `/files/?path=${encodeURIComponent(filePath(target))}`);
        }
        clearSelectionSilent();
        await loadFiles();
        // 可逆性：删除进回收站，提示条给一次撤销机会。
        toast(t("toast.deleted", { n: targets.length }), "info", {
          label: t("action.undo"),
          run: async () => {
            for (const p of before) {
              const entry2 = state.entries.find((x) => filePath(x) === p);
              if (entry2 && entry2.id) {
                await api("POST", "/files/restore", { id: entry2.id }).catch(() => {});
              }
            }
            await loadFiles();
            toast(t("toast.restored"));
          },
        });
      },
    });
  }

  function clearSelectionSilent() {
    state.selected.clear();
    state.anchor = null;
  }

  function openShareDialog(entry) {
    const path = filePath(entry);
    const isDir = entry.kind === "dir";

    formDialog({
      title: t("dlg.shareTitle", { name: entry.name }),
      fields: [
        {
          name: "kind",
          label: t("dlg.shareType"),
          type: "select",
          options: [
            { value: "link", label: t("dlg.shareLink") },
            ...(isDir ? [{ value: "request", label: t("dlg.shareRequest") }] : []),
          ],
        },
        { name: "password", label: t("dlg.password"), type: "password" },
        { name: "expires_hours", label: t("dlg.expires"), type: "number", min: "0" },
        { name: "max_uses", label: t("dlg.maxUses"), type: "number", min: "0" },
      ],
      confirm: t("dlg.createLink"),
      onSubmit: async (v) => {
        const payload = { path, kind: v.kind };
        if (v.password) payload.password = v.password;
        const hours = parseInt(v.expires_hours, 10);
        if (hours > 0) payload.expires_in_seconds = hours * 3600;
        const max = parseInt(v.max_uses, 10);
        if (max > 0) {
          if (v.kind === "request") payload.max_uploads = max;
          else payload.max_downloads = max;
        }

        const created = await api("POST", "/shares/", payload);
        const token = created.token;
        const url = token ? `${location.origin}/s/${token}` : "";

        if (url) {
          await navigator.clipboard.writeText(url).catch(() => {});
          formDialog({
            title: t("dlg.linkCreated"),
            body: t("dlg.linkCreatedBody"),
            fields: [],
            confirm: t("action.done"),
            extra: el("div", { class: "copybox", text: url }),
            onSubmit: async () => {},
          });
        }
        reloadDetail(entry);
      },
    });
  }

  // ---------- 分享视图 ----------

  function renderShares() {
    const wrap = el("div", { class: "scroll" });

    if (state.entries.length === 0) {
      return renderState("shares", t("state.sharesEmpty"), t("state.sharesEmptyHint"));
    }

    wrap.append(
      el("div", { class: "toolbar-row" }, [
        el("span", { class: "micro caption", text: t("shares.caption") }),
      ]),
    );

    const table = el("table", { class: "files table-plain" });
    const head = el("thead", {}, [
      el("tr", {}, [
        el("th", { scope: "col", text: t("shares.target") }),
        el("th", { scope: "col", text: t("shares.kind") }),
        el("th", { scope: "col", text: t("shares.uses") }),
        el("th", { scope: "col", text: t("shares.expires") }),
        el("th", { scope: "col", text: "" }),
      ]),
    ]);
    table.append(head);

    const body = el("tbody");
    for (const share of state.entries) {
      const uses =
        share.kind === "request"
          ? `${share.upload_count}${share.max_uploads ? " / " + share.max_uploads : ""}`
          : `${share.download_count}${share.max_downloads ? " / " + share.max_downloads : ""}`;

      body.append(
        el("tr", {}, [
          el("td", { text: share.node_path || share.label || "—" }),
          el("td", {}, [el("span", { class: "pill" + (share.kind === "request" ? " accent" : ""), text: share.kind })]),
          el("td", { class: "mono", text: uses }),
          el("td", { class: "mono", text: share.expires_at ? new Date(share.expires_at).toLocaleDateString(lang) : t("shares.never") }),
          el("td", { class: "cell-end" }, [
            el("button", {
              class: "row-action danger",
              text: t("action.revoke"),
              onclick: async () => {
                try {
                  await api("DELETE", `/shares/${share.id}`);
                  toast(t("toast.shareRevoked"));
                  loadShares();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        ]),
      );
    }
    table.append(body);
    wrap.append(table);
    return wrap;
  }

  async function loadShares() {
    renderSkeleton();
    try {
      const data = await api("GET", "/shares/");
      state.entries = data.items || [];
      refresh();
    } catch (err) {
      renderError(err);
    }
  }

  // ---------- 回收站 ----------

  function renderTrash() {
    const wrap = el("div", { class: "scroll" });

    if (state.entries.length === 0) {
      return renderState("trash", t("state.trashEmpty"), t("state.trashEmptyHint"));
    }

    const table = el("table", { class: "files table-plain" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { scope: "col", text: t("list.name") }),
          el("th", { scope: "col", text: t("list.size") }),
          el("th", { scope: "col", text: "" }),
        ]),
      ]),
    );
    const body = el("tbody");
    for (const entry of state.entries) {
      body.append(
        el("tr", {}, [
          el("td", {}, [el("span", { text: entry.name })]),
          el("td", { class: "mono", text: entry.kind === "dir" ? "—" : humanSize(entry.size) }),
          el("td", { class: "cell-end" }, [
            el("button", {
              class: "row-action",
              text: t("action.restore"),
              onclick: async () => {
                try {
                  await api("POST", "/files/restore", { id: entry.id });
                  toast(t("toast.restored"));
                  loadTrash();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        ]),
      );
    }
    table.append(body);
    wrap.append(table);
    return wrap;
  }

  async function loadTrash() {
    renderSkeleton();
    try {
      const data = await api("GET", "/files/trash");
      state.entries = data.items || [];
      refresh();
    } catch (err) {
      renderError(err);
    }
  }

  // ---------- 活动 ----------

  function personLabel(ev) {
    if (ev.actor_email) return ev.actor_email;
    if (ev.share_id) return t("audit.personViaLink");
    if (ev.actor_kind === "anonymous" || !ev.actor_kind) return t("audit.personAnonymous");
    if (ev.actor_kind === "system") return t("audit.personSystem");
    return ev.actor_kind;
  }

  function describeTarget(ev) {
    const name = ev.metadata && (ev.metadata.name || ev.metadata.path);
    if (name) return String(name);
    if (ev.share_id) return ev.share_id.slice(0, 8);
    if (ev.target_id) return ev.target_id.slice(0, 8);
    return "";
  }

  function renderAudit() {
    const shell = el("div", { class: "flex-col-fill" });
    const a = state.audit;

    const actionSelect = el("select", { "aria-label": t("audit.action"), onchange: (e) => { a.action = e.target.value; reloadAudit(); } });
    actionSelect.append(el("option", { value: "", text: t("audit.allActions") }));
    for (const action of a.actions) {
      const opt = el("option", { value: action, text: action });
      if (action === a.action) opt.selected = true;
      actionSelect.append(opt);
    }

    const resultSelect = el("select", { "aria-label": t("audit.result"), onchange: (e) => { a.result = e.target.value; reloadAudit(); } });
    for (const [value, label] of [["", t("audit.anyResult")], ["ok", t("audit.allowed")], ["denied", t("audit.refused")]]) {
      const opt = el("option", { value, text: label });
      if (value === a.result) opt.selected = true;
      resultSelect.append(opt);
    }

    const sinceSelect = el("select", { "aria-label": t("audit.since"), onchange: (e) => { a.since = e.target.value; reloadAudit(); } });
    for (const [value, label] of [
      ["", t("audit.allTime")],
      ["1h", t("audit.lastHour")],
      ["24h", t("audit.last24")],
      ["7d", t("audit.last7")],
      ["30d", t("audit.last30")],
    ]) {
      const opt = el("option", { value, text: label });
      if (value === a.since) opt.selected = true;
      sinceSelect.append(opt);
    }

    shell.append(
      el("div", { class: "toolbar-row" }, [
        actionSelect,
        resultSelect,
        sinceSelect,
        el("span", { class: "grow" }),
        el("span", { class: "mono mono-sm", text: t("count.events", { n: a.total }) }),
        el("button", {
          class: "btn ghost small",
          onclick: () => {
            const q = new URLSearchParams({ format: "csv" });
            if (a.action) q.set("actions", a.action);
            if (a.result) q.set("result", a.result);
            const since = sinceISO(a.since);
            if (since) q.set("since", since);
            location.href = `/api/audit?${q.toString()}`;
          },
        }, [svgIcon("download", 12), el("span", { text: t("audit.export") })]),
        iconButton("refresh", t("state.retry"), () => reloadAudit(), { iconSize: 13 }),
      ]),
    );

    const scroll = el("div", { class: "scroll" });

    if (a.events.length === 0) {
      scroll.append(
        renderState(
          "pulse",
          a.loading ? t("insp.loading") : t("state.activityEmpty"),
          a.action || a.result || a.since ? t("state.activityFiltered") : t("state.activityEmptyHint"),
        ),
      );
      shell.append(scroll);
      return shell;
    }

    const timeline = el("div", { class: "timeline" });
    let lastDay = "";

    for (const ev of a.events) {
      const day = dayOf(ev.ts);
      if (day !== lastDay) {
        lastDay = day;
        timeline.append(el("div", { class: "timeline-day", text: day }));
      }

      const target = describeTarget(ev);
      const meta = el("div", { class: "meta" });
      if (ev.actor_ip) meta.append(el("span", { text: ev.actor_ip }));
      if (ev.bytes) meta.append(el("span", { text: humanSize(ev.bytes) }));
      if (ev.share_id) meta.append(el("span", { text: ev.share_id.slice(0, 8) }));

      timeline.append(
        el("div", { class: `ev ${ev.result === "denied" ? "denied" : "ok"}` }, [
          el("div", { class: "when" }, [
            el("span", { class: "clock", text: clockOf(ev.ts) }),
            ev.result === "denied" ? t("audit.refused") : t("audit.allowed"),
          ]),
          el("div", { class: "rail", "aria-hidden": "true" }, [el("span", { class: "dot" })]),
          el("div", { class: "body" }, [
            el("div", { class: "headline" }, [
              el("span", { class: "action", text: ev.action }),
              target ? el("span", { class: "target", text: target, title: target }) : null,
              el("span", { class: "who", text: personLabel(ev) }),
            ]),
            meta.childNodes.length > 0 ? meta : null,
          ]),
        ]),
      );
    }

    scroll.append(timeline);
    scroll.append(
      el("div", { class: "center-actions" }, [
        a.hasMore
          ? el("button", {
              class: "btn",
              text: t("audit.loadOlder"),
              onclick: async () => {
                state.audit.offset += state.audit.events.length;
                await loadAudit(true);
              },
            })
          : el("span", { class: "micro", text: t("audit.end") }),
      ]),
    );

    shell.append(scroll);
    return shell;
  }

  function sinceISO(choice) {
    if (!choice) return "";
    const seconds = { "1h": 3600, "24h": 86400, "7d": 604800, "30d": 2592000 }[choice];
    if (!seconds) return "";
    return new Date(Date.now() - seconds * 1000).toISOString();
  }

  function reloadAudit() {
    state.audit.events = [];
    state.audit.offset = 0;
    loadAudit(false);
  }

  async function loadAudit(append) {
    const a = state.audit;
    if (a.loading) return;
    a.loading = true;

    const q = new URLSearchParams();
    q.set("offset", String(append ? a.offset : 0));
    if (a.action) q.set("actions", a.action);
    if (a.result) q.set("result", a.result);
    const since = sinceISO(a.since);
    if (since) q.set("since", since);

    try {
      const data = await api("GET", `/audit/events?${q.toString()}`);
      a.events = append ? a.events.concat(data.events || []) : data.events || [];
      a.total = data.total || 0;
      a.hasMore = !!data.has_more;
      a.actions = data.actions || [];
      if (!append) a.offset = 0;
    } catch (err) {
      fail(err);
    } finally {
      a.loading = false;
      refresh();
    }
  }

  // ---------- 访问 ----------

  function renderAccess() {
    const wrap = el("div", { class: "scroll" });

    wrap.append(
      el("div", { class: "pad" }, [
        el("div", { class: "card" }, [
          el("h2", { text: t("access.title") }),
          el("p", { class: "hint", text: t("access.body") }),
          el("div", { class: "copybox", text: `${location.origin}/dav/${state.me ? state.me.username : "you"}/` }),
          el("div", { class: "pad-top" }, [
            el("button", { class: "btn primary", onclick: () => createAppPassword() }, [
              svgIcon("key", 13),
              el("span", { text: t("dlg.newPassword") }),
            ]),
          ]),
        ]),
      ]),
    );

    if (state.entries.length === 0) return wrap;

    const table = el("table", { class: "files table-plain" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { scope: "col", text: t("access.label") }),
          el("th", { scope: "col", text: t("access.scope") }),
          el("th", { scope: "col", text: t("access.lastUsed") }),
          el("th", { scope: "col", text: "" }),
        ]),
      ]),
    );
    const body = el("tbody");
    for (const cred of state.entries) {
      body.append(
        el("tr", {}, [
          el("td", { text: cred.label || "—" }),
          el("td", {}, [el("span", { class: "pill", text: cred.scope })]),
          el("td", { class: "mono", text: cred.last_used ? humanTime(cred.last_used) : t("access.never") }),
          el("td", { class: "cell-end" }, [
            el("button", {
              class: "row-action danger",
              text: t("action.revoke"),
              onclick: async () => {
                try {
                  await api("DELETE", `/apppasswords/${cred.id}`);
                  toast(t("toast.revoked"));
                  loadCredentials();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        ]),
      );
    }
    table.append(body);
    wrap.append(table);
    return wrap;
  }

  async function loadCredentials() {
    renderSkeleton();
    try {
      const data = await api("GET", "/apppasswords/");
      state.entries = data.items || [];
      refresh();
    } catch (err) {
      renderError(err);
    }
  }

  function createAppPassword() {
    formDialog({
      title: t("dlg.newPassword"),
      fields: [
        { name: "label", label: t("dlg.label"), type: "text", placeholder: "laptop, phone, rclone" },
        {
          name: "scope",
          label: t("dlg.scope"),
          type: "select",
          options: [
            { value: "rw", label: t("dlg.scopeRW") },
            { value: "ro", label: t("dlg.scopeRO") },
          ],
        },
      ],
      confirm: t("action.create"),
      onSubmit: async (v) => {
        const created = await api("POST", "/apppasswords/", v);
        formDialog({
          title: t("dlg.passwordOnce"),
          body: t("dlg.passwordOnceBody"),
          fields: [],
          confirm: t("action.done"),
          extra: el("div", { class: "copybox", text: created.secret || "" }),
          onSubmit: async () => {
            await loadCredentials();
          },
        });
      },
    });
  }

  // ---------- 分享落地页 ----------

  async function renderShareLanding(token) {
    const root = $("#app");
    clear(root);

    let info;
    try {
      const resp = await fetch(`/s/${encodeURIComponent(token)}`, { headers: { Accept: "application/json" } });
      if (!resp.ok) throw new Error("unavailable");
      info = await resp.json();
    } catch {
      root.append(
        el("div", { class: "share-page" }, [
          el("div", { class: "share-card" }, [
            el("div", { class: "head" }, [
              el("div", { class: "mark" }),
              el("div", { class: "title", text: t("share.notAvailable") }),
              el("div", { class: "meta", text: t("share.notAvailableHint") }),
            ]),
          ]),
        ]),
      );
      return;
    }

    const card = el("div", { class: "share-card" });
    const head = el("div", { class: "head" }, [
      el("div", { class: "mark" }),
      el("div", { class: "title", text: info.label || t("nav.shares") }),
    ]);
    card.append(head);

    const body = el("div", { class: "body" });

    if (info.requires_password) {
      head.append(el("div", { class: "meta", text: t("share.passwordProtected") }));
      const input = el("input", { type: "password", placeholder: t("share.password"), "aria-label": t("share.password") });
      const submit = async () => {
        try {
          const resp = await fetch(`/s/${encodeURIComponent(token)}/verify`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ password: input.value }),
          });
          if (!resp.ok) {
            toast(t("share.wrongPassword"), "error");
            return;
          }
          renderShareLanding(token);
        } catch {
          toast(t("share.wrongPassword"), "error");
        }
      };
      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") submit();
      });
      body.append(
        el("label", { class: "field" }, [el("span", { class: "micro", text: t("share.password") }), input]),
        el("button", { class: "btn primary", text: t("share.continue"), onclick: submit }),
      );
    } else if (info.requires_email) {
      head.append(el("div", { class: "meta", text: t("share.emailLimited") }));
      const email = el("input", { type: "email", placeholder: "you@example.com", "aria-label": t("share.email") });
      const code = el("input", { type: "text", placeholder: t("share.code"), "aria-label": t("share.code") });

      body.append(
        el("label", { class: "field" }, [el("span", { class: "micro", text: t("share.email") }), email]),
        el("button", {
          class: "btn",
          text: t("share.sendCode"),
          class: "btn block",
          onclick: async () => {
            try {
              await fetch(`/s/${encodeURIComponent(token)}/recipient`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ email: email.value.trim() }),
              });
              toast(t("share.codeSent"));
            } catch {
              toast(t("share.codeBad"), "error");
            }
          },
        }),
        el("label", { class: "field" }, [el("span", { class: "micro", text: t("share.code") }), code]),
        el("button", {
          class: "btn primary",
          text: t("share.open"),
          onclick: async () => {
            try {
              const resp = await fetch(`/s/${encodeURIComponent(token)}/recipient/verify`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ email: email.value.trim(), code: code.value.trim() }),
              });
              if (!resp.ok) {
                toast(t("share.codeBad"), "error");
                return;
              }
              renderShareLanding(token);
            } catch {
              toast(t("share.codeBad"), "error");
            }
          },
        }),
      );
    } else if (info.kind === "request") {
      head.append(
        el("div", {
          class: "meta",
          text: info.max_uploads
            ? t("share.slots", { left: Math.max(0, info.max_uploads - info.upload_count), max: info.max_uploads })
            : t("share.uploadOne"),
        }),
      );

      const input = el("input", { type: "file", "aria-label": t("share.file") });
      const button = el("button", {
        class: "btn primary",
        text: t("share.upload"),
        onclick: async () => {
          if (!input.files.length) return;
          const form = new FormData();
          form.append("file", input.files[0]);
          button.disabled = true;
          try {
            const resp = await fetch(`/s/${encodeURIComponent(token)}/upload`, { method: "POST", body: form });
            const payload = await resp.json().catch(() => ({}));
            if (!resp.ok) throw new Error(payload.message || "");
            toast(payload.name || "");
            renderShareLanding(token);
          } catch (err) {
            toast(err.message, "error");
            button.disabled = false;
          }
        },
      });

      body.append(
        el("label", { class: "field" }, [el("span", { class: "micro", text: t("share.file") }), input]),
        button,
      );
    } else if (!info.can_read) {
      body.append(el("p", { class: "hint", text: t("share.cannotRead") }));
    } else {
      body.append(
        el("button", {
          class: "btn primary",
          text: t("share.download"),
          onclick: () => {
            location.href = `/s/${encodeURIComponent(token)}/download`;
          },
        }),
      );

      if (info.children && info.children.length > 0) {
        const list = el("div", { class: "share-list" });
        for (const child of info.children) {
          const row = el("div", { class: "row" }, [
            el("span", { class: "name", text: child.name }),
            el("span", { class: "size", text: child.kind === "dir" ? "" : humanSize(child.size) }),
          ]);
          if (child.kind !== "dir") {
            row.append(
              el("a", {
                href: `/s/${encodeURIComponent(token)}/download?name=${encodeURIComponent(child.name)}`,
                text: t("share.download"),
                class: "link-sm",
              }),
            );
          }
          list.append(row);
        }
        body.append(list);
      }
    }

    card.append(body);
    card.append(
      el("div", { class: "foot" }, [
        el("span", { text: t("share.sharedWith") }),
        el("span", { text: info.expires_at ? t("share.expires", { date: new Date(info.expires_at).toLocaleDateString(lang) }) : "" }),
      ]),
    );
    root.append(el("div", { class: "share-page" }, [card]));
  }

  // ---------- 路由 ----------

  function switchTo(view) {
    state.view = view;
    state.entries = [];
    state.filter = "";
    clearSelectionSilent();
    state.detail = { path: null, versions: null, shares: null, loading: false };
    refresh();

    switch (view) {
      case "shares":
        loadShares();
        break;
      case "trash":
        loadTrash();
        break;
      case "activity":
        reloadAudit();
        break;
      case "access":
        loadCredentials();
        break;
      default:
        loadFiles();
    }
  }

  // ---------- 键盘 ----------

  function installKeyboard() {
    document.addEventListener("keydown", (e) => {
      const tag = (e.target && e.target.tagName) || "";
      const typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
      const dialogOpen = document.querySelector("dialog[open]");

      // ⌘K 在输入框里也要生效：命令面板本来就是从任何地方唤起的。
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        openPalette();
        return;
      }

      if (state.paletteOpen) return;
      if (dialogOpen) return;
      if (typing) {
        if (e.key === "Escape" && e.target.id === "filter") {
          state.filter = "";
          e.target.value = "";
          renderBody();
          renderStatus();
        }
        return;
      }

      if (e.key === "/") {
        e.preventDefault();
        const filter = $("#filter");
        if (filter) filter.focus();
        return;
      }

      if (state.view !== "files") return;

      if (e.key === "Backspace") {
        e.preventDefault();
        if (state.path) navigate(state.path.split("/").slice(0, -1).join("/"));
        return;
      }
      if (e.key === "Escape" && state.selected.size > 0) {
        e.preventDefault();
        clearSelection();
        return;
      }

      const current = inspectedEntry();
      if (e.key === "F2" && current) {
        e.preventDefault();
        promptRename(current);
      } else if (e.key === "Delete" && current) {
        e.preventDefault();
        confirmDelete(current);
      } else if (e.key === " " && current) {
        e.preventDefault();
        if (current.preview) openPreview(current);
      }
    });
  }

  // ---------- 拖放 ----------

  function installDragAndDrop() {
    const zone = $("#dropzone");
    const target = $("#drop-target");
    let depth = 0;

    const carriesFiles = (e) => e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");

    window.addEventListener("dragenter", (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      depth++;
      if (state.view !== "files") return;
      if (target) target.textContent = state.path ? `/ ${state.path}` : "/";
      zone.classList.add("active");
    });

    window.addEventListener("dragover", (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    });

    // dragleave 会在指针进入子元素时触发，所以用深度计数而不是布尔值，否则浮层会在指针划过
    // 列表时闪烁。
    window.addEventListener("dragleave", (e) => {
      if (!carriesFiles(e)) return;
      depth--;
      if (depth <= 0) {
        depth = 0;
        zone.classList.remove("active");
      }
    });

    window.addEventListener("drop", (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      depth = 0;
      zone.classList.remove("active");
      if (state.view !== "files") return;
      if (e.dataTransfer.files && e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
    });
  }

  // ---------- 在线状态 ----------

  function installConnectivity() {
    const update = () => {
      state.online = navigator.onLine !== false;
      refresh();
    };
    window.addEventListener("online", () => {
      state.online = true;
      refresh();
      toast(t("status.online"));
    });
    window.addEventListener("offline", () => {
      state.online = false;
      refresh();
    });
    update();
  }

  // ---------- 启动 ----------

  async function boot() {
    applyLang();
    applyTheme();
    applyDensity();
    applyRail();

    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
      if (state.themePref === "auto") applyTheme();
    });

    const shareMatch = location.pathname.match(/^\/s\/([^/]+)\/?$/);
    if (shareMatch) {
      await renderShareLanding(shareMatch[1]);
      return;
    }

    try {
      state.me = await api("GET", "/auth/me");
    } catch (err) {
      if (err instanceof APIError && err.status === 401) {
        location.href = "/api/auth/login?next=" + encodeURIComponent(location.pathname);
        return;
      }
      throw err;
    }

    installDragAndDrop();
    installKeyboard();
    installConnectivity();

    const params = new URLSearchParams(location.search);
    if (params.get("path")) state.path = params.get("path").replace(/^\/+|\/+$/g, "");

    refresh();
    await loadFiles();
  }

  document.addEventListener("DOMContentLoaded", boot);
})();
