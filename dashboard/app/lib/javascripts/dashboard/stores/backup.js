import { extend } from 'marbles/utils';
import Store from '../store';
import Config from '../config';
import Dispatcher from '../dispatcher';

var Backup = Store.createClass({
	displayName: "Stores.Backup",

	getState: function () {
		return this.state;
	},

	willInitialize: function () {},

	getInitialState: function () {
		return {
			downloading: false,
			err: null,
			size: null,
			url: null,
			filename: null
		};
	},

	didBecomeActive: function () {},

	didBecomeInactive: function () {
		this.constructor.discardInstance(this);
	},

	handleEvent: function (event) {
		switch (event.name) {
		case 'DOWNLOAD_BACKUP':
			this.getBackup();
			break;
		}
	},

	getBackup: function () {
		this.setState(extend(this.getInitialState(), {
			downloading: true
		}));
		var url = Config.endpoints.cluster_controller +'/backup?key='+ encodeURIComponent(Config.user.controller_key);
		var xhr = new XMLHttpRequest();
		xhr.onreadystatechange = function () {
			if (xhr.readyState === 4) {
				this.setState({
					downloading: false,
					err: xhr.status === 200 ? null : 'Something went wrong',
					size: xhr.response.size,
					url: URL.createObjectURL(xhr.response),
					filename: ((xhr.getResponseHeader('Content-Disposition') || '').match(/filename=['"]([^'"]+)['"]/) || [])[1] || 'flynn-backup.tar'
				});
			}
		}.bind(this);
		xhr.onprogress = function (e) {
			this.setState({
				size: e.loaded
			});
		}.bind(this);
		xhr.responseType = 'blob';
		xhr.open('GET', url, true);
		xhr.send();
	}
});

Backup.isValidId = function (id) {
	return id === null;
};

Backup.dispatcherIndex = Backup.registerWithDispatcher(Dispatcher);

export default Backup;
