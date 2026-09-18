// The Daycore server this extension points at before anyone configures one.
//
// ⚠️ It is only a DEFAULT: the options page always wins, and whatever is saved
// there lives per Chrome profile in chrome.storage.sync. Change this one line
// to make a fresh install land on your own deployment instead of localhost.
//
// ⚠️ ONE SOURCE ON PURPOSE. This value used to be written twice — once in
// options.js and once inline in popup.js — and the popup's copy is the one that
// actually decides where a push goes when storage is empty (first run, or after
// a profile wipe). Two copies of a default is two chances for them to disagree.
const DAYCORE_DEFAULT_URL = "https://daycore.xfcloud.org";
