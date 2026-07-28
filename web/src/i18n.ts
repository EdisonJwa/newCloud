// Internationalisation.
//
// Messages are looked up by key and interpolated as whole templates. No string concatenation
// anywhere: word order differs between languages, and a sentence assembled from fragments
// cannot be translated. Plurals go through Intl.PluralRules, numbers and dates through Intl,
// so nothing here assumes a decimal separator, a 12-hour clock or a week start.

import { createContext, useContext } from "react";

export type Lang = "en" | "zh-Hans";

type Table = Record<string, string>;

const en: Table = {
  "app.name": "newcloud",

  "nav.home": "Home",
  "nav.files": "Files",
  "nav.shared": "Shared",
  "nav.activity": "Activity",
  "nav.trash": "Trash",
  "nav.access": "Access",
  "nav.signout": "Sign out",
  "nav.sections": "Sections",

  "home.continue": "Pick up where you left off",
  "home.continueEmpty": "No recent files yet.",
  "home.sharedByYou": "Shared by you",
  "home.sharedEmpty": "Nothing is shared yet.",
  "home.space": "This space",
  "home.used": "Used",
  "home.quota": "Quota",
  "home.files": "Files",
  "home.folders": "Folders",
  "home.openFiles": "Browse files",
  "home.expires": "expires {date}",
  "home.link": "link",
  "home.request": "drop box",
  "home.uses": "{used} of {max}",
  "home.unlimited": "unlimited",

  "files.name": "Name",
  "files.size": "Size",
  "files.modified": "Modified",
  "files.emptyTitle": "This folder is empty",
  "files.emptyHint": "Drop files here, or use Upload.",
  "files.upload": "Upload",
  "files.newFolder": "New folder",
  "files.filter": "Filter",
  "files.up": "Up one level",
  "files.noMatch": "Nothing matches",
  "files.clearFilter": "Clear filter",
  "files.dropHere": "Drop to upload",
  "files.dropInto": "into /{path}",
  "files.items_one": "{n} item",
  "files.items_other": "{n} items",
  "files.truncated": "Showing the first {n}; this folder has more.",

  "detail.title": "Details",
  "detail.close": "Close details",
  "detail.kind": "Kind",
  "detail.size": "Size",
  "detail.modified": "Modified",
  "detail.type": "Type",
  "detail.path": "Path",
  "detail.open": "Open",
  "detail.download": "Download",
  "detail.share": "Share",
  "detail.rename": "Rename",
  "detail.delete": "Delete",
  "detail.versions": "Versions",
  "detail.versionsEmpty": "No earlier versions.",
  "detail.restore": "Restore",
  "detail.shared": "Links",
  "detail.sharedEmpty": "Not shared.",
  "detail.linkNotRecoverable": "URL not recoverable",
  "detail.noPreview": "No preview for this type",

  "kind.folder": "Folder",
  "kind.image": "Image",
  "kind.video": "Video",
  "kind.audio": "Audio",
  "kind.pdf": "PDF",
  "kind.text": "Text",
  "kind.file": "File",

  "preview.close": "Close preview",
  "preview.prev": "Previous file",
  "preview.next": "Next file",
  "preview.unavailable": "Preview unavailable",
  "preview.imageFailed": "This image could not be displayed.",
  "preview.mediaFailed": "The browser cannot play this format.",
  "preview.none": "No preview for this type.",
  "preview.download": "Download",

  "shared.title": "Shared links",
  "shared.subtitle": "Each link can be limited by password, expiry and use count.",
  "shared.empty": "Nothing is shared yet.",
  "shared.emptyHint": "Open a file and choose Share to create a link.",
  "shared.target": "Target",
  "shared.kind": "Kind",
  "shared.uses": "Uses",
  "shared.expires": "Expires",
  "shared.never": "never",
  "shared.revoke": "Revoke",

  "activity.title": "Activity",
  "count.events_one": "{n} event",
  "count.events_other": "{n} events",
  "activity.action": "Action",
  "activity.result": "Result",
  "activity.since": "Time",
  "activity.allActions": "All actions",
  "activity.anyResult": "Any result",
  "activity.allowed": "allowed",
  "activity.refused": "refused",
  "activity.allTime": "All time",
  "activity.lastHour": "Last hour",
  "activity.last24": "Last 24 hours",
  "activity.last7": "Last 7 days",
  "activity.last30": "Last 30 days",
  "activity.export": "Export CSV",
  "activity.loadOlder": "Load older",
  "activity.end": "End of the log",
  "activity.empty": "No activity yet.",
  "activity.filtered": "Nothing matches these filters.",
  "activity.anonymous": "anonymous",
  "activity.viaLink": "via link",
  "activity.system": "system",

  "trash.title": "Trash",
  "trash.subtitle": "Deleted files keep their content and can be restored.",
  "trash.empty": "Trash is empty.",
  "trash.emptyHint": "Deleted files appear here.",
  "trash.restore": "Restore",

  "access.title": "Access",
  "access.webdav": "WebDAV endpoint",
  "access.webdavHint":
    "Use an app password for WebDAV, rclone or a mounted drive. Each can be revoked on its own, so a leaked one does not end your browser session.",
  "access.newPassword": "New app password",
  "access.label": "Label",
  "access.scope": "Scope",
  "access.scopeRW": "Read and write",
  "access.scopeRO": "Read only",
  "access.lastUsed": "Last used",
  "access.never": "never",
  "access.empty": "No app passwords.",
  "access.secretTitle": "Copy this now",
  "access.secretBody": "It cannot be shown again. Use your username with this value as the password.",

  "palette.placeholder": "Search files and commands…",
  "palette.commands": "Commands",
  "palette.files": "Files",
  "palette.empty": "No matches",
  "palette.goHome": "Go to Home",
  "palette.goFiles": "Go to Files",
  "palette.goShared": "Go to Shared",
  "palette.goActivity": "Go to Activity",
  "palette.goTrash": "Go to Trash",
  "palette.goAccess": "Go to Access",
  "palette.upload": "Upload files",
  "palette.newFolder": "New folder",
  "palette.theme": "Switch theme",
  "palette.lang": "Switch language",
  "palette.nav": "↑↓ to move",
  "palette.run": "Enter to run",
  "palette.close": "Esc to close",

  "dlg.cancel": "Cancel",
  "dlg.create": "Create",
  "dlg.done": "Done",
  "dlg.rename": "Rename",
  "dlg.renameLabel": "New name",
  "dlg.newFolder": "New folder",
  "dlg.folderLabel": "Name",
  "dlg.deleteOne": "Delete {name}?",
  "dlg.deleteMany": "Delete {n} items?",
  "dlg.deleteBody": "They move to the trash and can be restored.",
  "dlg.shareTitle": "Share {name}",
  "dlg.shareKind": "Type",
  "dlg.shareLink": "Link — anyone with the URL can download",
  "dlg.shareRequest": "Drop box — others can upload into this folder",
  "dlg.password": "Password (optional)",
  "dlg.expires": "Expires in hours (optional)",
  "dlg.maxUses": "Maximum uses (optional)",
  "dlg.createLink": "Create link",
  "dlg.linkReady": "Link created",
  "dlg.linkReadyBody": "Copied to the clipboard. The token is not shown again.",

  "toast.uploaded_one": "Uploaded {n} file",
  "toast.uploaded_other": "Uploaded {n} files",
  "toast.uploadSome": "Uploaded {done}, {failed} failed",
  "toast.uploading": "An upload is already running",
  "toast.deleted_one": "Moved to trash",
  "toast.deleted_other": "Moved {n} items to trash",
  "toast.renamed": "Renamed",
  "toast.folderCreated": "Folder created",
  "toast.restored": "Restored",
  "toast.revoked": "Revoked",
  "toast.linkCopied": "Link copied",
  "toast.versionRestored": "Restored version {n}",

  "state.errorTitle": "Something went wrong",
  "state.retry": "Try again",
  "state.deniedTitle": "No access",
  "state.deniedHint": "You do not have permission to view this.",
  "state.offline": "Offline — changes cannot be saved.",
  "state.loading": "Loading…",

  "theme.auto": "Auto",
  "theme.light": "Light",
  "theme.dark": "Dark",
  "theme.hc": "High contrast",
  "theme.label": "Theme",
  "lang.label": "Language",

  "share.notAvailable": "Link not available",
  "share.notAvailableHint": "It may have expired, been revoked, or reached its usage limit.",
  "share.passwordProtected": "This link is protected by a password.",
  "share.emailLimited": "This link is limited to specific addresses.",
  "share.password": "Password",
  "share.continue": "Continue",
  "share.wrongPassword": "Wrong password",
  "share.email": "Your email",
  "share.code": "Code",
  "share.sendCode": "Send me a code",
  "share.codeSent": "If that address was invited, a code is on its way",
  "share.codeBad": "That code was not accepted",
  "share.open": "Open",
  "share.download": "Download",
  "share.upload": "Upload",
  "share.file": "File",
  "share.slots": "{left} of {max} slots left",
  "share.uploadOne": "Upload a file to send it.",
  "share.cannotRead": "This link cannot be downloaded.",
  "share.sharedWith": "Shared with newcloud",
};

const zh: Table = {
  "app.name": "newcloud",

  "nav.home": "首页",
  "nav.files": "文件",
  "nav.shared": "分享",
  "nav.activity": "活动",
  "nav.trash": "回收站",
  "nav.access": "访问",
  "nav.signout": "退出登录",
  "nav.sections": "分区",

  "home.continue": "接着上次",
  "home.continueEmpty": "还没有最近的文件。",
  "home.sharedByYou": "你分享的",
  "home.sharedEmpty": "还没有任何分享。",
  "home.space": "这个空间",
  "home.used": "已用",
  "home.quota": "配额",
  "home.files": "文件",
  "home.folders": "文件夹",
  "home.openFiles": "浏览文件",
  "home.expires": "{date} 过期",
  "home.link": "链接",
  "home.request": "投递箱",
  "home.uses": "{used} / {max}",
  "home.unlimited": "不限",

  "files.name": "名称",
  "files.size": "大小",
  "files.modified": "修改时间",
  "files.emptyTitle": "这个文件夹是空的",
  "files.emptyHint": "把文件拖进来，或使用“上传”。",
  "files.upload": "上传",
  "files.newFolder": "新建文件夹",
  "files.filter": "筛选",
  "files.up": "上一层",
  "files.noMatch": "没有匹配项",
  "files.clearFilter": "清除筛选",
  "files.dropHere": "松开即上传",
  "files.dropInto": "到 /{path}",
  "files.items_other": "{n} 项",
  "files.truncated": "只显示了前 {n} 项，此目录还有更多。",

  "detail.title": "详情",
  "detail.close": "关闭详情",
  "detail.kind": "种类",
  "detail.size": "大小",
  "detail.modified": "修改时间",
  "detail.type": "类型",
  "detail.path": "路径",
  "detail.open": "打开",
  "detail.download": "下载",
  "detail.share": "分享",
  "detail.rename": "重命名",
  "detail.delete": "删除",
  "detail.versions": "版本",
  "detail.versionsEmpty": "没有更早的版本。",
  "detail.restore": "恢复",
  "detail.shared": "链接",
  "detail.sharedEmpty": "未被分享。",
  "detail.linkNotRecoverable": "URL 不可再取回",
  "detail.noPreview": "此类型没有预览",

  "kind.folder": "文件夹",
  "kind.image": "图片",
  "kind.video": "视频",
  "kind.audio": "音频",
  "kind.pdf": "PDF",
  "kind.text": "文本",
  "kind.file": "文件",

  "preview.close": "关闭预览",
  "preview.prev": "上一个",
  "preview.next": "下一个",
  "preview.unavailable": "预览不可用",
  "preview.imageFailed": "这张图片无法显示。",
  "preview.mediaFailed": "浏览器无法播放这个格式。",
  "preview.none": "此类型没有预览。",
  "preview.download": "下载",

  "shared.title": "分享链接",
  "shared.subtitle": "每个链接都可以限制密码、有效期与使用次数。",
  "shared.empty": "还没有任何分享。",
  "shared.emptyHint": "选中文件后点“分享”即可创建链接。",
  "shared.target": "目标",
  "shared.kind": "类型",
  "shared.uses": "使用",
  "shared.expires": "过期",
  "shared.never": "不过期",
  "shared.revoke": "吊销",

  "activity.title": "活动",
  "count.events_other": "{n} 条",
  "activity.action": "动作",
  "activity.result": "结果",
  "activity.since": "时间",
  "activity.allActions": "全部动作",
  "activity.anyResult": "任意结果",
  "activity.allowed": "允许",
  "activity.refused": "拒绝",
  "activity.allTime": "全部时间",
  "activity.lastHour": "最近 1 小时",
  "activity.last24": "最近 24 小时",
  "activity.last7": "最近 7 天",
  "activity.last30": "最近 30 天",
  "activity.export": "导出 CSV",
  "activity.loadOlder": "加载更早",
  "activity.end": "日志到头了",
  "activity.empty": "暂无活动。",
  "activity.filtered": "没有符合筛选条件的事件。",
  "activity.anonymous": "匿名",
  "activity.viaLink": "通过链接",
  "activity.system": "系统",

  "trash.title": "回收站",
  "trash.subtitle": "删除的文件保留内容，可以恢复。",
  "trash.empty": "回收站是空的。",
  "trash.emptyHint": "删除的文件会出现在这里。",
  "trash.restore": "恢复",

  "access.title": "访问",
  "access.webdav": "WebDAV 地址",
  "access.webdavHint":
    "WebDAV、rclone 或挂载盘请使用应用密码。每个都可以单独吊销，泄露一个不会影响浏览器会话。",
  "access.newPassword": "新建应用密码",
  "access.label": "标签",
  "access.scope": "范围",
  "access.scopeRW": "读写",
  "access.scopeRO": "只读",
  "access.lastUsed": "最近使用",
  "access.never": "从未",
  "access.empty": "没有应用密码。",
  "access.secretTitle": "现在就复制",
  "access.secretBody": "它不会再次显示。用户名照常，密码填这个值。",

  "palette.placeholder": "搜索文件与命令…",
  "palette.commands": "命令",
  "palette.files": "文件",
  "palette.empty": "没有匹配项",
  "palette.goHome": "前往首页",
  "palette.goFiles": "前往文件",
  "palette.goShared": "前往分享",
  "palette.goActivity": "前往活动",
  "palette.goTrash": "前往回收站",
  "palette.goAccess": "前往访问",
  "palette.upload": "上传文件",
  "palette.newFolder": "新建文件夹",
  "palette.theme": "切换主题",
  "palette.lang": "切换语言",
  "palette.nav": "↑↓ 移动",
  "palette.run": "回车执行",
  "palette.close": "Esc 关闭",

  "dlg.cancel": "取消",
  "dlg.create": "创建",
  "dlg.done": "完成",
  "dlg.rename": "重命名",
  "dlg.renameLabel": "新名称",
  "dlg.newFolder": "新建文件夹",
  "dlg.folderLabel": "名称",
  "dlg.deleteOne": "删除 {name}？",
  "dlg.deleteMany": "删除 {n} 项？",
  "dlg.deleteBody": "它们会进入回收站，可以从那里恢复。",
  "dlg.shareTitle": "分享 {name}",
  "dlg.shareKind": "类型",
  "dlg.shareLink": "链接 — 拿到 URL 的人都能下载",
  "dlg.shareRequest": "投递箱 — 别人可向此文件夹上传",
  "dlg.password": "密码（可选）",
  "dlg.expires": "有效期小时数（可选）",
  "dlg.maxUses": "使用次数上限（可选）",
  "dlg.createLink": "创建链接",
  "dlg.linkReady": "链接已创建",
  "dlg.linkReadyBody": "已复制到剪贴板。令牌不会再次显示。",

  "toast.uploaded_other": "已上传 {n} 个文件",
  "toast.uploadSome": "已上传 {done} 个，{failed} 个失败",
  "toast.uploading": "已有上传在进行",
  "toast.deleted_other": "已移入回收站",
  "toast.renamed": "已重命名",
  "toast.folderCreated": "文件夹已创建",
  "toast.restored": "已恢复",
  "toast.revoked": "已吊销",
  "toast.linkCopied": "链接已复制",
  "toast.versionRestored": "已恢复版本 {n}",

  "state.errorTitle": "出错了",
  "state.retry": "重试",
  "state.deniedTitle": "没有权限",
  "state.deniedHint": "你没有查看此对象的权限。",
  "state.offline": "离线中——无法保存更改。",
  "state.loading": "加载中…",

  "theme.auto": "跟随系统",
  "theme.light": "明亮",
  "theme.dark": "暗色",
  "theme.hc": "高对比",
  "theme.label": "主题",
  "lang.label": "语言",

  "share.notAvailable": "链接不可用",
  "share.notAvailableHint": "它可能已过期、已被吊销，或已达使用上限。",
  "share.passwordProtected": "此链接受密码保护。",
  "share.emailLimited": "此链接仅限指定邮箱访问。",
  "share.password": "密码",
  "share.continue": "继续",
  "share.wrongPassword": "密码错误",
  "share.email": "你的邮箱",
  "share.code": "验证码",
  "share.sendCode": "发验证码给我",
  "share.codeSent": "如果该地址在邀请名单内，验证码已发出",
  "share.codeBad": "验证码未被接受",
  "share.open": "打开",
  "share.download": "下载",
  "share.upload": "上传",
  "share.file": "文件",
  "share.slots": "还剩 {left} / {max} 个名额",
  "share.uploadOne": "上传一个文件发送过去。",
  "share.cannotRead": "此链接不可下载。",
  "share.sharedWith": "由 newcloud 分享",
};

const TABLES: Record<Lang, Table> = { en, "zh-Hans": zh };

export const LANGS: Lang[] = ["en", "zh-Hans"];

function detectLang(): Lang {
  const stored = localStorageSafe("wc.lang");
  if (stored === "en" || stored === "zh-Hans") return stored;
  for (const tag of navigator.languages ?? [navigator.language ?? "en"]) {
    const lower = tag.toLowerCase();
    if (lower.startsWith("zh")) return "zh-Hans";
    if (lower.startsWith("en")) return "en";
  }
  return "en";
}

function localStorageSafe(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export interface I18n {
  lang: Lang;
  setLang: (lang: Lang) => void;
  /** Translate. `n` drives plural selection; other params are interpolated. */
  t: (key: string, params?: Record<string, string | number>) => string;
  fmt: {
    size: (bytes: number) => string;
    relative: (iso: string) => string;
    full: (iso: string) => string;
    clock: (iso: string) => string;
    day: (iso: string) => string;
    date: (iso: string) => string;
  };
}

export function createI18n(lang: Lang): I18n {
  const table = TABLES[lang];
  const plural = new Intl.PluralRules(lang);
  const nums = new Intl.NumberFormat(lang);
  const sizeFmt = new Intl.NumberFormat(lang, { maximumFractionDigits: 1 });
  const rel = new Intl.RelativeTimeFormat(lang, { numeric: "auto" });
  const full = new Intl.DateTimeFormat(lang, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
  const clock = new Intl.DateTimeFormat(lang, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
  const dayFmt = new Intl.DateTimeFormat(lang, { month: "short", day: "numeric" });
  const dateFmt = new Intl.DateTimeFormat(lang, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });

  const t = (key: string, params?: Record<string, string | number>): string => {
    let template: string | undefined;
    if (params && typeof params.n === "number") {
      const form = plural.select(params.n);
      template = table[`${key}_${form}`] ?? table[`${key}_other`] ?? table[`${key}_one`];
    }
    template ??= table[key] ?? en[key] ?? key;

    if (!params) return template;
    return template.replace(/\{(\w+)\}/g, (whole, name: string) => {
      const value = params[name];
      if (value === undefined) return whole;
      return typeof value === "number" ? nums.format(value) : String(value);
    });
  };

  const humanSize = (bytes: number): string => {
    if (!bytes) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
    const value = bytes / Math.pow(1024, i);
    return `${i === 0 ? Math.round(value) : sizeFmt.format(value)} ${units[i]}`;
  };

  return {
    lang,
    setLang: () => {},
    t,
    fmt: {
      size: humanSize,
      relative: (iso) => {
        if (!iso) return "";
        const seconds = (Date.now() - new Date(iso).getTime()) / 1000;
        if (seconds < 60) return rel.format(0, "second");
        if (seconds < 3600) return rel.format(-Math.floor(seconds / 60), "minute");
        if (seconds < 86400) return rel.format(-Math.floor(seconds / 3600), "hour");
        if (seconds < 604800) return rel.format(-Math.floor(seconds / 86400), "day");
        return dateFmt.format(new Date(iso));
      },
      full: (iso) => (iso ? full.format(new Date(iso)) : ""),
      clock: (iso) => clock.format(new Date(iso)),
      day: (iso) => dayFmt.format(new Date(iso)),
      date: (iso) => dateFmt.format(new Date(iso)),
    },
  };
}

export const I18nContext = createContext<I18n>(createI18n(detectLang()));

export function useI18n(): I18n {
  return useContext(I18nContext);
}

export { detectLang };
