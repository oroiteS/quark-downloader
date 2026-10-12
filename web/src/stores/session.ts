// 会话 store（Task 3，zustand）：登录态 + 账户信息 + 解析页草稿保留。
//
// loggedIn 三态语义：null=初始未知（启动首拉 /api/account 前）、true/false
// 明确态。pendingParseText 服务 Review Focus #3——auth expired / 401 跳登录
// 时解析页已输入文本保在内存不丢：logoutReset() 刻意不清它。
import { create } from 'zustand';
import type { Capacity } from '../api/types';

interface SessionState {
  loggedIn: boolean | null;
  nickname: string;
  avatar: string;
  capacity: Capacity | null;
  /** 解析页草稿（Focus #3：会话过期跳登录不丢）。 */
  pendingParseText: string;
  setLoggedIn: (v: boolean) => void;
  setAccount: (account: {
    nickname: string;
    avatar: string;
    capacity: Capacity;
  }) => void;
  setPendingParseText: (text: string) => void;
  /** 登出/过期重置：清登录态与账户字段，保留 pendingParseText。 */
  logoutReset: () => void;
}

export const useSession = create<SessionState>()((set) => ({
  loggedIn: null,
  nickname: '',
  avatar: '',
  capacity: null,
  pendingParseText: '',
  setLoggedIn: (v) => set({ loggedIn: v }),
  setAccount: ({ nickname, avatar, capacity }) =>
    set({ loggedIn: true, nickname, avatar, capacity }),
  setPendingParseText: (text) => set({ pendingParseText: text }),
  logoutReset: () =>
    set({ loggedIn: false, nickname: '', avatar: '', capacity: null }),
}));
